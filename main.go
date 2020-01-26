package main

import (
	"context"
	json "github.com/json-iterator/go"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/websocket"

	"fmt"

	"path/filepath"

	ggr "github.com/aerokube/ggr/config"
	"github.com/aerokube/selenoid/config"
	"github.com/aerokube/selenoid/protect"
	"github.com/aerokube/selenoid/service"
	"github.com/aerokube/selenoid/session"
	"github.com/aerokube/selenoid/upload"
	"github.com/aerokube/util"
	"github.com/aerokube/util/docker"
	"github.com/docker/docker/client"
	"github.com/spf13/viper"
	_ "github.com/spf13/viper/remote"
)

var (
	hostname                 string
	disableDocker            bool
	disableQueue             bool
	enableFileUpload         bool
	listen                   string
	timeout                  time.Duration
	maxTimeout               time.Duration
	newSessionAttemptTimeout time.Duration
	sessionDeleteTimeout     time.Duration
	serviceStartupTimeout    time.Duration
	gracefulPeriod           time.Duration
	limit                    int
	retryCount               int
	containerNetwork         string
	sessions                 = session.NewMap()
	confPath                 string
	logConfPath              string
	captureDriverLogs        bool
	disablePrivileged        bool
	videoOutputDir           string
	videoRecorderImage       string
	logOutputDir             string
	ggrHostEnv               string
	saveAllLogs              bool
	configuration            *viper.Viper
	consulAddr, consulPath   string
	ggrHost                  *ggr.Host
	conf                     *config.Config
	queue                    *protect.Queue
	manager                  service.Manager
	cli                      *client.Client

	startTime = time.Now()

	gitRevision = "HEAD"
	buildStamp  = "unknown"
)

func init() {

	configuration = viper.New()
	consulAddr = os.Getenv("CONSUL_URL")
	consulPath = os.Getenv("CONSUL_PATH")

	if consulAddr == "" {
		configuration.SetConfigName("config")
		configuration.SetConfigType("yaml")
		configuration.AddConfigPath(".")
		err := configuration.ReadInConfig()
		if err != nil {
			log.Fatalf("Failed reading config: %v", err)
		}
	} else {
		err := configuration.AddRemoteProvider("consul", consulAddr, consulPath)
		if err != nil {
			log.Fatalf("Failed configuring consul connection: %v", err)
		}
		configuration.SetConfigType("yaml")
		err = configuration.ReadRemoteConfig()
		if err != nil {
			log.Fatalf("Failed connecting to consul: %v", err)
		}
	}

	go func() {
		for {
			time.Sleep(time.Second * 30)
			if consulAddr == "" {
				err := configuration.ReadInConfig()
				if err != nil {
					log.Fatalf("Failed reading config: %v", err)
				}
			} else {
				err := configuration.ReadRemoteConfig()
				if err != nil {
					log.Printf("unable to read remote config: %v", err)
					continue
				}
			}
		}
	}()

	disableDocker = configuration.GetBool("selenoid.disable-docker")
	disableQueue = configuration.GetBool("selenoid.disable-queue")
	enableFileUpload = configuration.GetBool("selenoid.enable-file-upload")
	listen = configuration.GetString("selenoid.listen")
	confPath = configuration.GetString("selenoid.conf")
	logConfPath = configuration.GetString("selenoid.log-conf")
	if configuration.GetBool("selenoid.limit-auto") {
		limit = configuration.GetInt("selenoid.limit")
	}
	retryCount = configuration.GetInt("selenoid.retry-count")
	timeout = time.Second * configuration.GetDuration("selenoid.timeout")
	maxTimeout = time.Hour * configuration.GetDuration("selenoid.max-timeout")
	newSessionAttemptTimeout = time.Second * configuration.GetDuration("selenoid.session-attempt-timeout")
	sessionDeleteTimeout = time.Second * configuration.GetDuration("selenoid.session-delete-timeout")
	serviceStartupTimeout = time.Second * configuration.GetDuration("selenoid.service-startup-timeout")
	containerNetwork = configuration.GetString("selenoid.container-network")
	captureDriverLogs = configuration.GetBool("selenoid.captureDriverLogs")
	disablePrivileged = configuration.GetBool("selenoid.disable-privileged")
	videoOutputDir = configuration.GetString("selenoid.video-output-dir")
	videoRecorderImage = configuration.GetString("selenoid.video-recorder-image")
	logOutputDir = configuration.GetString("selenoid.log-output-dir")
	ggrHostEnv = configuration.GetString("selenoid.ggr-host")
	saveAllLogs = configuration.GetBool("selenoid.save-all-logs")
	gracefulPeriod = time.Second * configuration.GetDuration("selenoid.graceful-period")

	var err error
	hostname, err = os.Hostname()
	if err != nil {
		log.Fatalf("[-] [INIT] [%s: %v]", os.Args[0], err)
	}
	if ggrHostEnv != "" {
		ggrHost = parseGgrHost(ggrHostEnv)
	}
	queue = protect.New(limit, disableQueue)
	conf = config.NewConfig()
	err = conf.Load(confPath, logConfPath)
	if err != nil {
		log.Fatalf("[-] [INIT] [%s: %v]", os.Args[0], err)
	}
	onSIGHUP(func() {
		err := conf.Load(confPath, logConfPath)
		if err != nil {
			log.Printf("[-] [INIT] [%s: %v]", os.Args[0], err)
		}
	})
	inDocker := false
	_, err = os.Stat("/.dockerenv")
	if err == nil {
		inDocker = true
	}

	if !disableDocker {
		videoOutputDir, err = filepath.Abs(videoOutputDir)
		if err != nil {
			log.Fatalf("[-] [INIT] [Invalid video output dir %s: %v]", videoOutputDir, err)
		}
		err = os.MkdirAll(videoOutputDir, os.FileMode(0644))
		if err != nil {
			log.Fatalf("[-] [INIT] [Failed to create video output dir %s: %v]", videoOutputDir, err)
		}
		log.Printf("[-] [INIT] [Video Dir: %s]", videoOutputDir)
	}
	if logOutputDir != "" {
		logOutputDir, err = filepath.Abs(logOutputDir)
		if err != nil {
			log.Fatalf("[-] [INIT] [Invalid log output dir %s: %v]", logOutputDir, err)
		}
		err = os.MkdirAll(logOutputDir, os.FileMode(0644))
		if err != nil {
			log.Fatalf("[-] [INIT] [Failed to create log output dir %s: %v]", logOutputDir, err)
		}
		log.Printf("[-] [INIT] [Logs Dir: %s]", logOutputDir)
		if saveAllLogs {
			log.Printf("[-] [INIT] [Saving all logs]")
		}
	}

	upload.Init()

	environment := service.Environment{
		InDocker:             inDocker,
		CPU:                  configuration.GetInt64("selenoid.cpu"),
		Memory:               configuration.GetInt64("selenoid.mem"),
		Network:              containerNetwork,
		StartupTimeout:       serviceStartupTimeout,
		SessionDeleteTimeout: sessionDeleteTimeout,
		CaptureDriverLogs:    captureDriverLogs,
		VideoOutputDir:       videoOutputDir,
		VideoContainerImage:  videoRecorderImage,
		LogOutputDir:         logOutputDir,
		SaveAllLogs:          saveAllLogs,
		Privileged:           !disablePrivileged,
	}
	if disableDocker {
		manager = &service.DefaultManager{Environment: &environment, Config: conf}
		if logOutputDir != "" && captureDriverLogs {
			log.Fatalf("[-] [INIT] [In drivers mode only one of -capture-driver-logs and -log-output-dir flags is allowed]")
		}
		return
	}
	dockerHost := os.Getenv("DOCKER_HOST")
	if dockerHost == "" {
		dockerHost = client.DefaultDockerHost
	}
	u, err := client.ParseHostURL(dockerHost)
	if err != nil {
		log.Fatalf("[-] [INIT] [%v]", err)
	}
	ip, _, _ := net.SplitHostPort(u.Host)
	environment.IP = ip
	cli, err = docker.CreateCompatibleDockerClient(
		func(specifiedApiVersion string) {
			log.Printf("[-] [INIT] [Using Docker API version: %s]", specifiedApiVersion)
		},
		func(determinedApiVersion string) {
			log.Printf("[-] [INIT] [Your Docker API version is %s]", determinedApiVersion)
		},
		func(defaultApiVersion string) {
			log.Printf("[-] [INIT] [Did not manage to determine your Docker API version - using default version: %s]", defaultApiVersion)
		},
	)
	if err != nil {
		log.Fatalf("[-] [INIT] [New docker client: %v]", err)
	}
	manager = &service.DefaultManager{Environment: &environment, Client: cli, Config: conf}
}

func parseGgrHost(s string) *ggr.Host {
	h, p, err := net.SplitHostPort(s)
	if err != nil {
		log.Fatalf("[-] [INIT] [Invalid Ggr host: %v]", err)
	}
	ggrPort, err := strconv.Atoi(p)
	if err != nil {
		log.Fatalf("[-] [INIT] [Invalid Ggr host: %v]", err)
	}
	host := &ggr.Host{
		Name: h,
		Port: ggrPort,
	}
	log.Printf("[-] [INIT] [Will prefix all session IDs with a hash-sum: %s]", host.Sum())
	return host
}

func onSIGHUP(fn func()) {
	sig := make(chan os.Signal)
	signal.Notify(sig, syscall.SIGHUP)
	go func() {
		for {
			<-sig
			fn()
		}
	}()
}

var seleniumPaths = struct {
	CreateSession, ProxySession string
}{
	CreateSession: "/session",
	ProxySession:  "/session/",
}

func selenium() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(seleniumPaths.CreateSession, queue.Try(queue.Check(queue.Protect(post(create)))))
	mux.HandleFunc(seleniumPaths.ProxySession, proxy)
	mux.HandleFunc(paths.Status, status)
	mux.HandleFunc(paths.Welcome, welcome)
	return mux
}

func post(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	}
}

func ping(w http.ResponseWriter, _ *http.Request) {
	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Uptime         string `json:"uptime"`
		LastReloadTime string `json:"lastReloadTime"`
		NumRequests    uint64 `json:"numRequests"`
		Version        string `json:"version"`
	}{time.Since(startTime).String(), conf.LastReloadTime.Format(time.RFC3339), getSerial(), gitRevision})
}

func video(w http.ResponseWriter, r *http.Request) {
	requestId := serial()
	if r.Method == http.MethodDelete {
		deleteFileIfExists(requestId, w, r, videoOutputDir, paths.Video, "DELETED_VIDEO_FILE")
		return
	}
	user, remote := util.RequestInfo(r)
	if _, ok := r.URL.Query()[jsonParam]; ok {
		listFilesAsJson(requestId, w, videoOutputDir, "VIDEO_ERROR")
		return
	}
	log.Printf("[%d] [VIDEO_LISTING] [%s] [%s]", requestId, user, remote)
	fileServer := http.StripPrefix(paths.Video, http.FileServer(http.Dir(videoOutputDir)))
	fileServer.ServeHTTP(w, r)
}

func deleteFileIfExists(requestId uint64, w http.ResponseWriter, r *http.Request, dir string, prefix string, status string) {
	user, remote := util.RequestInfo(r)
	fileName := strings.TrimPrefix(r.URL.Path, prefix)
	filePath := filepath.Join(dir, fileName)
	_, err := os.Stat(filePath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Unknown file %s", filePath), http.StatusNotFound)
		return
	}
	err = os.Remove(filePath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to delete file %s: %v", filePath, err), http.StatusInternalServerError)
		return
	}
	log.Printf("[%d] [%s] [%s] [%s] [%s]", requestId, status, user, remote, fileName)
}

var paths = struct {
	Video, VNC, Logs, Devtools, Download, Clipboard, File, Ping, Status, Error, WdHub, Welcome string
}{
	Video:     "/video/",
	VNC:       "/vnc/",
	Logs:      "/logs/",
	Devtools:  "/devtools/",
	Download:  "/download/",
	Clipboard: "/clipboard/",
	Status:    "/status",
	File:      "/file",
	Ping:      "/ping",
	Error:     "/error",
	WdHub:     "/wd/hub",
	Welcome:   "/",
}

func handler() http.Handler {
	root := http.NewServeMux()
	root.HandleFunc(paths.WdHub+"/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-Type", "application/json")
		r.URL.Scheme = "http"
		r.URL.Host = (&request{r}).localaddr()
		r.URL.Path = strings.TrimPrefix(r.URL.Path, paths.WdHub)
		selenium().ServeHTTP(w, r)
	})
	root.HandleFunc(paths.Error, func(w http.ResponseWriter, r *http.Request) {
		util.JsonError(w, "Session timed out or not found", http.StatusNotFound)
	})
	root.HandleFunc(paths.Status, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-Type", "application/json")
		json.NewEncoder(w).Encode(conf.State(sessions, limit, queue.Queued(), queue.Pending()))
	})
	root.HandleFunc(paths.Ping, ping)
	root.Handle(paths.VNC, websocket.Handler(vnc))
	root.HandleFunc(paths.Logs, logs)
	root.HandleFunc(paths.Video, video)
	root.HandleFunc(paths.Download, reverseProxy(func(sess *session.Session) string { return sess.HostPort.Fileserver }, "DOWNLOADING_FILE"))
	root.HandleFunc(paths.Clipboard, reverseProxy(func(sess *session.Session) string { return sess.HostPort.Clipboard }, "CLIPBOARD"))
	root.HandleFunc(paths.Devtools, reverseProxy(func(sess *session.Session) string { return sess.HostPort.Devtools }, "DEVTOOLS"))
	if enableFileUpload {
		root.HandleFunc(paths.File, fileUpload)
	}
	root.HandleFunc(paths.Welcome, welcome)
	return root
}

func showVersion() {
	fmt.Printf("Git Revision: %s\n", gitRevision)
	fmt.Printf("UTC Build Time: %s\n", buildStamp)
}

func main() {
	log.Printf("[-] [INIT] [Timezone: %s]", time.Local)
	log.Printf("[-] [INIT] [Listening on %s]", listen)

	stop := make(chan os.Signal)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	server := &http.Server{
		Addr:    listen,
		Handler: handler(),
	}
	e := make(chan error)
	go func() {
		e <- server.ListenAndServe()
	}()
	select {
	case err := <-e:
		log.Fatalf("[-] [INIT] [Failed to start: %v]", err)
	case <-stop:
	}

	log.Printf("[-] [SHUTTING_DOWN] [%s]", gracefulPeriod)
	ctx, cancel := context.WithTimeout(context.Background(), gracefulPeriod)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("[-] [SHUTTING_DOWN] [Failed to shut down: %v]", err)
	}

	sessions.Each(func(k string, s *session.Session) {
		if enableFileUpload {
			os.RemoveAll(path.Join(os.TempDir(), k))
		}
		s.Cancel()
	})

	if !disableDocker {
		err := cli.Close()
		if err != nil {
			log.Fatalf("[-] [SHUTTING_DOWN] [Error closing Docker client: %v]", err)
		}
	}
}
