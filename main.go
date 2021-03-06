package main

import (
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/go-github/github"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/ssh"
)

type config struct {
	Listen     string `mapstructure:"listen"`
	Port       string `mapstructure:"port"`
	RetryCount int    `mapstructure:"retry_count"`
	RetryDelay int    `mapstructure:"retry_delay"`
	Initialise bool   `mapstructure:"initialise"`
	Threads    int    `mapstructure:"threads"`
	Logging    logger
	Logfile    *os.File
	LastUpdate time.Time
	Repos      []repo
	DataPasser *DataPasser
}

type logger struct {
	Format    string `mapstructure:"format"`
	Output    string `mapstructure:"output"`
	Level     string `mapstructure:"level"`
	Timestamp bool   `mapstructure:"timestamp"`
}

type repo struct {
	URL           string `mapstructure:"url"`
	Path          string `mapstructure:"path"`
	Directory     string `mapstructure:"directory"`
	Label         string `mapstructure:"label"`
	LabelType     string `mapstructure:"labelType"`
	Remote        string `mapstructure:"remote"`
	Secret        string `mapstructure:"secret"`
	SSHPrivKey    string `mapstructure:"sshPrivKey"`
	SSHPassPhrase string `mapstructure:"sshPassPhrase"`
	Trigger       string `mapstructure:"trigger"`
	Busy          bool   // when clone / update
}

type job struct {
	repo    *repo
	jobType string
}

// DataPasser - A way to pass extra arguments into http.HandleFunc
type DataPasser struct {
	jobs    chan *job
	threads int
}

// C is global config
var C config
var mutex sync.Mutex
var log = logrus.New()

func (c *config) FindRepo(path string) (int, bool) {
	for r, repo := range c.Repos {
		if repo.Path == cleanURL(path) {
			return r, true
		}
	}
	return 0, false
}

func cleanURL(url string) string {
	// strip trailing slash
	if url[len(url)-1] == '/' {
		return url[:len(url)-1]
	}
	return url
}

func (r *repo) finished() {
	r.Busy = false
}

func (r *repo) waitForCompletion() {
	rlog := log.WithFields(logrus.Fields{
		"repo":      r.Name(),
		"path":      r.Path,
		"label":     r.Label,
		"labelType": r.LabelType,
	})
	// check if update already in progress and let it finish
	for {
		if r.Busy {
			rlog.Warnln("Repo is in the middle of an update, waiting...")
			time.Sleep(3 * time.Second)
		} else {
			break
		}
	}
}

func (r *repo) clone() {
	defer r.finished()
	rlog := log.WithFields(logrus.Fields{
		"repo":      r.Name(),
		"path":      r.Path,
		"label":     r.Label,
		"labelType": r.LabelType,
	})

	r.waitForCompletion()
	r.Busy = true
	sshAuth, err := ssh.NewPublicKeysFromFile("git", r.SSHPrivKey, r.SSHPassPhrase)
	if err != nil {
		rlog.Errorf("Failed to setup ssh auth: %v", err)
		return
	}

	var ref string
	if r.LabelType == "tag" {
		ref = "refs/tags/" + r.Label
	} else {
		ref = "refs/heads/" + r.Label
	}

	rlog.Debugf("Clone reference: %v", ref)

	// checkout specific branch / tag
	_, err = git.PlainClone(r.Directory, false, &git.CloneOptions{
		URL:           r.URL,
		ReferenceName: plumbing.ReferenceName(ref),
		Auth:          sshAuth,
	})

	if err != nil {
		rlog.Errorf("Failed to clone repository: %v", err)
		return
	}

	rlog.Info("Cloned repository")

	r.touchTrigger()
}

// essentially git fetch and git reset --hard origin/master | latest remote commit
func (r *repo) update() {
	defer r.finished()
	rlog := log.WithFields(logrus.Fields{
		"repo":      r.Name(),
		"path":      r.Path,
		"remote":    r.Remote,
		"label":     r.Label,
		"labelType": r.LabelType,
	})

	r.waitForCompletion()
	r.Busy = true
	sshAuth, err := ssh.NewPublicKeysFromFile("git", r.SSHPrivKey, r.SSHPassPhrase)
	if err != nil {
		rlog.Errorf("Failed to setup ssh auth: %v", err)
		return
	}

	repo, err := git.PlainOpen(r.Directory)
	if err != nil {
		rlog.Errorf("Failed to open local git repository: %v", err)
		return
	}

	w, err := repo.Worktree()
	if err != nil {
		rlog.Errorf("Failed to open work tree for repository: %v", err)
		return
	}

	// fetches from github can be flaky, sometimes we get a blank .git/refs/remotes/[master|branch name],
	// and complaints about broken refs, subsequent fetches should fix this!
	// we'll fetch up to the retry amount until it succeeds!.

	for i := 0; i < C.RetryCount; i++ {
		rlog.Info("Fetch attempt: ", i+1)
		err = repo.Fetch(&git.FetchOptions{
			RemoteName: r.Remote,
			Auth:       sshAuth,
			Force:      true,
			Tags:       git.AllTags,
		})
		if err == nil {
			break
		}
		if err == git.NoErrAlreadyUpToDate {
			rlog.Info("No new commits")
			return
		}
		if err != nil {
			rlog.Errorf("Failed to fetch updates: %v", err)
			time.Sleep(time.Duration(C.RetryDelay) * time.Second)
			continue
		}
	}
	rlog.Info("Fetched new updates")

	var ref string
	if r.LabelType == "tag" {
		ref = "refs/tags/" + r.Label
	} else {
		ref = "refs/remotes/" + r.Remote + "/" + r.Label
	}

	var targetHash plumbing.Hash
	remoteRef, err := repo.Reference(plumbing.ReferenceName(ref), true)
	if err != nil {
		rlog.Errorf("Failed to get reference for %s: %v", ref, err)
		return
	}

	targetHash = remoteRef.Hash()

	// test if annotated tag and amend targetHash
	if atag, err := repo.TagObject(remoteRef.Hash()); err == nil {
		rlog.Infof("Annotated tag hash: %v", atag.Hash)
		rlog.Infof("Annotated tag target hash: %v", atag.Target)
		targetHash = atag.Target
	}

	localRef, err := repo.Reference(plumbing.ReferenceName("HEAD"), true)
	if err != nil {
		rlog.Errorf("Failed to get local reference for HEAD: %v", err)
		return
	}

	if remoteRef.Hash() == localRef.Hash() {
		rlog.Warning("Already up to date")
		return
	}

	// git reset --hard [origin/master|hash] - works for both branch and tag, we'll reset direct to the hash
	err = w.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: targetHash})
	if err != nil {
		rlog.Errorf("Failed to hard reset work tree: %v", err)
		return
	}
	rlog.Info("Hard reset successful, confirming changes....")
	headRef, err := repo.Reference(plumbing.ReferenceName("HEAD"), true)
	if err != nil {
		rlog.Errorf("Failed to get local HEAD reference: %v", err)
		return
	}

	if headRef.Hash() == targetHash {
		rlog.Infof("Changes confirmed, latest hash: %v", headRef.Hash())
	} else {
		rlog.Error("Something went wrong, hashes don't match!")
		rlog.Debugf("Remote hash: %v", targetHash)
		rlog.Debugf("Local hash:  %v", headRef.Hash())
		return
	}

	r.touchTrigger()
}

func (r *repo) touchTrigger() {
	rlog := log.WithFields(logrus.Fields{
		"repo":      r.Name(),
		"path":      r.Path,
		"label":     r.Label,
		"labelType": r.LabelType,
	})
	if r.HasTrigger() {
		if err := os.Chtimes(r.Trigger, time.Now(), time.Now()); err != nil {
			rlog.Errorf("Failed to update trigger file: %v, attempting to create...", err)

			// attempt to create trigger file silently, reports error but creates empty file
			os.OpenFile(r.Trigger, os.O_RDONLY|os.O_CREATE, 0660)
			if _, err := os.Stat(r.Trigger); err != nil {
				rlog.Errorf("Failed to create trigger file: %v", err)
			}
			rlog.Info("Successfully created trigger file")
			return
		}
		rlog.Info("Successfully updated trigger file")
	}
}

func (c *config) validatePathsUniq() {
	paths := make(map[string]bool)

	for _, r := range c.Repos {
		if _, ok := paths[r.Path]; ok {
			// duplicate found
			log.Errorf("Multiple repos found with the same path: %v, please correct, only the first instance will be used otherwise", r.Path)
		}
		paths[r.Path] = true
	}
}

// short name for the logs
func (r *repo) Name() string {
	return strings.TrimSuffix((strings.TrimPrefix(r.URL, "git@github.com:")), ".git")
}

func isEmpty(field string) bool {
	if len(field) == 0 {
		return true
	}
	return false
}

func (r *repo) HasTrigger() bool {
	if isEmpty(r.Trigger) {
		return false
	}
	return true
}

func (r *repo) HasSecret() bool {
	if isEmpty(r.Secret) {
		return false
	}
	return true
}

func process(jobs chan *job, threads int) {
	sem := make(chan struct{}, threads)
	for {
		select {
		case j := <-jobs:
			go func() {
				sem <- struct{}{}
				switch j.jobType {
				case "clone":
					j.repo.clone()
				case "update":
					j.repo.update()
				}
				<-sem
			}()
		}
	}

}

func (p *DataPasser) handleFunc(w http.ResponseWriter, r *http.Request) {
	//func handler(w http.ResponseWriter, r *http.Request) {
	idx, ok := C.FindRepo(r.URL.Path)
	if !ok {
		log.Warnf("Repository not found for path: %v", r.URL.Path)
		return
	}

	payload, err := github.ValidatePayload(r, []byte(C.Repos[idx].Secret))
	defer r.Body.Close()
	if err != nil {
		log.Errorf("Error validating request body: %v", err)
		return
	}

	event, err := github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		log.Errorf("Could not parse webhook: %v", err)
		return
	}

	switch e := event.(type) {
	case *github.PushEvent:
		if C.Repos[idx].URL == *e.Repo.SSHURL && (C.Repos[idx].Label == strings.TrimPrefix(*e.Ref, "refs/heads/") || C.Repos[idx].Label == strings.TrimPrefix(*e.Ref, "refs/tags/")) {
			p.jobs <- &job{repo: &C.Repos[idx], jobType: "update"}
		} else {
			log.WithFields(logrus.Fields{
				"URL": *e.Repo.SSHURL,
				"Ref": *e.Ref,
			}).Warn("Push event did not match our configuration")
		}
		return
	default:
		log.Warnf("Unknown event type %v", github.WebHookType(r))
		return
	}
}

func (c *config) setRepoDefaults() {
	for i := range c.Repos {
		if c.Repos[i].LabelType == "" {
			c.Repos[i].LabelType = "branch"
		}
		if c.Repos[i].Label == "" {
			c.Repos[i].Label = "master"
		}
		if c.Repos[i].Remote == "" {
			c.Repos[i].Remote = "origin"
		}
	}
}

func (c *config) validateLabelType() {
	for i := range c.Repos {
		// either known or blank, if blank our setRepoDefaults function will set
		if c.Repos[i].LabelType == "branch" || c.Repos[i].LabelType == "tag" || c.Repos[i].LabelType == "" {
			continue
		} else {
			log.Warnf("Unknown label type for repo: %s, defaulting to branch", c.Repos[i].Name)
		}

	}
}

func (c *config) setLogging() {

	// inverse timestamp
	var dts bool
	if c.Logging.Timestamp {
		dts = false
	} else {
		dts = true
	}

	if c.Logging.Format == "" || c.Logging.Format == "text" {
		log.Formatter = &logrus.TextFormatter{FullTimestamp: true, DisableTimestamp: dts}
	} else {
		log.Formatter = &logrus.JSONFormatter{DisableTimestamp: dts}
	}

	switch c.Logging.Level {
	case "info":
		log.SetLevel(logrus.InfoLevel)
	case "debug":
		log.SetLevel(logrus.DebugLevel)
	case "warn":
		log.SetLevel(logrus.WarnLevel)
	case "error":
		log.SetLevel(logrus.ErrorLevel)
	default:
		log.SetLevel(logrus.InfoLevel)
	}
	// file or stdout
	if c.Logging.Output == "" || c.Logging.Output == "stdout" {
		if c.Logfile != nil {
			c.Logfile.Close()
			c.Logfile = nil
		}
		log.Out = os.Stdout
	} else {
		if c.Logfile != nil {
			if err := c.Logfile.Close(); err != nil {
				log.Errorf("Error closing logfile handle = %+v", err)
			}
		}
		file, err := os.OpenFile(c.Logging.Output, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0660)
		if err != nil {
			log.Out = os.Stdout
			log.Errorf("Failed to log to file, using default stdout: %v", err)
			return
		}
		c.Logfile = file
		log.Out = c.Logfile
	}
}

func (c *config) refreshTasks() {
	c.setLogging()
	c.validatePathsUniq()
	c.validateLabelType()
	c.setRepoDefaults()
	// TODO: respawn process()
	c.DataPasser.threads = c.Threads
	c.LastUpdate = time.Now()
}

func (c *config) initialClone() {
	if c.Initialise {
		for idx, r := range c.Repos {
			if _, err := os.Stat(r.Directory); err != nil {
				c.DataPasser.jobs <- &job{repo: &c.Repos[idx], jobType: "clone"}
			}
		}
	}

}

func main() {
	// setup config
	viper.SetConfigName("config")
	viper.AddConfigPath("/etc/gwg")
	viper.AddConfigPath(".")

	viper.SetDefault("listen", "0.0.0.0")
	viper.SetDefault("port", 5555)
	viper.SetDefault("retry_delay", 10)
	viper.SetDefault("retry_count", 1)
	viper.SetDefault("threads", 5)
	viper.SetDefault("initialise", true)
	viper.SetDefault("logging.format", "text")
	viper.SetDefault("logging.output", "stdout")
	viper.SetDefault("logging.level", "info")
	viper.SetDefault("logging.timestamp", true)

	if err := viper.ReadInConfig(); err != nil {
		log.Fatalf("Failed to read config file: %v", err)
	}
	if err := viper.Unmarshal(&C); err != nil {
		log.Fatalf("Failed to setup configuration: %v", err)
	}

	signalCh := make(chan os.Signal)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signalCh
		var busy bool
		log.Println("Signal received, preparing to shutting down...")
		for {
			// will C update on hot reload???
			busy = false

			// make a copy each loop so it gets new versions
			repos := C.Repos
			for _, r := range repos {
				if r.Busy {
					busy = true
				}
			}
			if busy {
				log.Warnln("Repo updates are still in progress (waiting to safely shutdown)...")
				time.Sleep(5 * time.Second)
			} else {
				log.Println("Shutting down GWG!")
				os.Exit(0)
			}
		}
	}()

	passer := &DataPasser{
		jobs:    make(chan *job, 100),
		threads: C.Threads,
	}

	C.DataPasser = passer

	C.refreshTasks()

	viper.WatchConfig()
	// event fired twice on linux but once on mac? wtf!!!
	viper.OnConfigChange(func(e fsnotify.Event) {
		mutex.Lock()
		if time.Since(C.LastUpdate).Nanoseconds() < 250229410 {
			return
		}

		// create entirely new config, set defaults and change 'C'
		// yaml and toml differences in repo mappings means we have to unmarshal
		// everything first.
		var newC config
		if err := viper.Unmarshal(&newC); err != nil {
			log.Fatalf("Failed to setup new configuration: %v", err)
		}

		log.Warnf("Config file changed: %v", e.Name)
		log.Debugf("Event: %v", e.Op)
		newC.DataPasser = passer
		newC.refreshTasks()

		// wait until repos are finished updating / cloning
		for {
			// will C update on hot reload???
			repoBusy := false

			// make a copy each loop so it gets new versions
			repos := C.Repos
			for _, r := range repos {
				if r.Busy {
					repoBusy = true
				}
			}
			if repoBusy {
				log.Println("Repo updates are still in progress (waiting to safely update configuration)...")
				time.Sleep(5 * time.Second)
			} else {
				log.Println("Replacing configuration...")
				// replace current config with new one
				C = newC
				break
			}
		}

		mutex.Unlock()
		log.Warn("Configuration updated")
	})

	go process(passer.jobs, passer.threads)

	C.initialClone()

	// Start the server.
	// (listen and port changes require a restart)
	//http.HandleFunc("/", handler)
	http.HandleFunc("/", passer.handleFunc)
	http.ListenAndServe(C.Listen+":"+C.Port, nil)

}
