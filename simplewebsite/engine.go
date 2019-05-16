package simplewebsite

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"

	// "encoding/base64"
	"compress/gzip"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/ugorji/go-common/logging"
	"github.com/ugorji/go-common/osutil"
	"github.com/ugorji/go-serverapp/web"
	// "github.com/ugorji/go-common/errorutil"
)

type DynamicPathFn func(s *Server, w http.ResponseWriter, r *http.Request) error

type engineInitCfg struct {
	Config         string
	BaseDir        string
	BaseRuntimeDir string
	DynamicPathFns map[string]DynamicPathFn
	// Watch          bool // Deprecated
}

type engineCfg struct {
	RuntimeDir        string
	PidFile           string // write pid to this file
	ExitPath          string // exit if url path equals this
	PingPath          string // just return ping path
	ReloadPath        string
	AccessLogFile     string
	ListenAddress     string
	MaxConcurrentConn int32
	NoGzip            bool
	Watch             bool
	ServerTemplate    Server
	Servers           []Server
}

type engineState struct {
	engineCfg
	watcher      *watcher
	reloadTime   time.Time
	websvr       *web.HTTPServer
	accessLogger *web.AccessLogger
	dynamicFns   map[string]DynamicPathFn
	// pidFileWritten bool
}

type Engine struct {
	// Note: All files/dir directly here are absolute paths

	closed         bool
	mu             sync.RWMutex
	baseDir        string // default basedir for config, pages, etc
	baseRuntimeDir string // default basedir for pid, access log, runtime data
	configFile     string // config file
	fatalErrChan   chan error
	engineState
}

// func newEngine(cfgFile string, dyn map[string]DynamicPathFn) (e *Engine, err error) {
func newEngine(c engineInitCfg) (e *Engine, err error) {
	e = new(Engine)

	e.configFile = c.Config
	e.dynamicFns = c.DynamicPathFns
	e.baseDir = c.BaseDir
	e.baseRuntimeDir = c.BaseRuntimeDir

	err = e.reload()
	return
}

func (e *Engine) reload() (err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return
	}

	// if e.configFile, err = filepath.Abs(e.configFile); err != nil {
	// 	return
	// }
	f, err := os.Open(e.configFile)
	if err != nil {
		return
	}
	defer f.Close()

	if e.fatalErrChan == nil {
		e.fatalErrChan = make(chan error)
	}

	var ecfg engineCfg
	if err = json.NewDecoder(f).Decode(&ecfg); err != nil {
		return
	}

	// assign defaults
	if ecfg.ServerTemplate.SMTPAddr == "" {
		ecfg.ServerTemplate.SMTPAddr = "127.0.0.0:25"
	}
	if ecfg.PidFile == "" {
		ecfg.PidFile = "simplewebsite.pid"
	}
	// AccessLogFile must be explicitly configured
	// if ecfg.AccessLogFile == "" {
	// 	ecfg.AccessLogFile = "simplewebsite.access.log")
	// }

	// base RuntimeDir, PidFile and AccessLogFile off baseRuntimeDir
	if !filepath.IsAbs(ecfg.RuntimeDir) {
		ecfg.RuntimeDir = filepath.Join(e.baseRuntimeDir, ecfg.RuntimeDir)
	}
	if !filepath.IsAbs(ecfg.PidFile) {
		ecfg.PidFile = filepath.Join(e.baseRuntimeDir, ecfg.PidFile)
	}
	if !filepath.IsAbs(ecfg.AccessLogFile) {
		ecfg.AccessLogFile = filepath.Join(e.baseRuntimeDir, ecfg.AccessLogFile)
	}

	if err = os.MkdirAll(ecfg.RuntimeDir, os.ModePerm); err != nil {
		return
	}

	if e.websvr != nil {
		e.websvr.HardPause()
		defer e.websvr.ResumeFromHardPause()
		e.websvr.WaitZeroInflight()
	}

	if e.watcher != nil {
		e.watcher.Close()
		e.watcher = nil
	}

	ecfg0 := e.engineCfg
	e.engineCfg = ecfg

	e.reloadTime = time.Now().UTC() // use UTC, so it works in last-mod headers also

	if ecfg0.PidFile != ecfg.PidFile {
		zpid := []byte(strconv.Itoa(os.Getpid()) + "\n")
		if err = osutil.WriteFile(ecfg.PidFile, zpid, false); err != nil {
			return
		}
	}

	for i := range e.Servers {
		if err = e.Servers[i].runInit(&e.engineCfg, e.baseDir); err != nil {
			return
		}
	}
	log.Notice(nil, "Engine Initialized in %v", time.Since(e.reloadTime))

	if ecfg.ListenAddress == "" {
		err = fmt.Errorf("%sEngine: No Listen Address Configured", errTag)
		return
	}

	var closeOldWebSvr, closeOldAccessLogger bool

	oldWebsvr := e.websvr
	oldAccessLog := e.accessLogger

	if ecfg.ListenAddress == ecfg0.ListenAddress {
		if ecfg.MaxConcurrentConn != ecfg0.MaxConcurrentConn {
			// change directly here, since we are already at hardpause, and 0 in-flight.
			e.websvr.ResetMaxNumConn(ecfg.MaxConcurrentConn)
		}
	} else if e.websvr != nil {
		e.websvr = nil
		closeOldWebSvr = true
	}
	if ecfg.AccessLogFile != ecfg0.AccessLogFile && e.accessLogger != nil {
		e.accessLogger = nil
		closeOldAccessLogger = true
	}

	if e.accessLogger == nil {
		e.accessLogger = web.NewAccessLogger(ecfg.AccessLogFile)
		if err = e.accessLogger.Reopen(); err != nil {
			return
		}
		if e.websvr != nil {
			e.websvr.Pipes[0] = e.accessLogger
		}
	}

	if e.websvr == nil {
		var httplis net.Listener
		if httplis, err = net.Listen("tcp", ecfg.ListenAddress); err != nil {
			return
		}
		e.websvr = &web.HTTPServer{
			Listener: web.NewListener(httplis, ecfg.MaxConcurrentConn, web.OnPanicAll),
		}
		e.websvr.Pipes = append(e.websvr.Pipes, e.accessLogger)
		if !ecfg.NoGzip {
			e.websvr.Pipes = append(e.websvr.Pipes,
				web.NewGzipPipe(gzip.DefaultCompression, 4, 20),
				web.NewBufferPipe(web.MinMimeSniffLen, 4, 20))
		}
		e.websvr.Pipes = append(e.websvr.Pipes, web.HttpHandlerPipe{Handler: e})
		go func() {
			e.fatalErrChan <- (&http.Server{Handler: e.websvr}).Serve(e.websvr)
		}()
	}

	// ecfg setup successfully. close old websvr now, and set engineState to ecfg
	if closeOldWebSvr || closeOldAccessLogger {
		go func() {
			if closeOldWebSvr {
				if zerr := oldWebsvr.Close(); zerr != nil {
					log.IfError(nil, zerr, "Error closing old webserver at %s", ecfg0.ListenAddress)
				}
			}
			if closeOldAccessLogger {
				if err = oldAccessLog.Close(); err != nil {
					return
				}
			}
		}()
	}

	if e.Watch {
		if err = newWatcher(e); err != nil { // 256 batches, 512 inotify events
			log.IfError(nil, err, "Error starting watch service")
			return
		}
	}
	// if e.watcher != nil {
	// 	if err = e.watcher.reload(); err != nil {
	// 		return
	// 	}
	// }
	log.Notice(nil, "Engine Now Listening at: %s", e.ListenAddress)
	// err = http.ListenAndServe(e.ListenAddress, e)
	return
}

func (e *Engine) Close() (err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	if e.websvr != nil {
		e.websvr.Close()
	}
	if e.accessLogger != nil {
		e.accessLogger.Close()
	}
	if e.watcher != nil {
		e.watcher.Close()
	}

	logging.Close()
	return
}

// If necessary, support closing pipes.
// Right now, all pipes used should not be closed explicitly.
// func (e *engineState) closePipes() error {
// 	var merr errorutil.Multi
// 	for _, p := range e.pipes {
// 		switch x := p.(type) {
// 		case *web.GzipPipe:
// 			merr = append(merr, x.Close())
// 		case *web.BufferPipe:
// 			merr = append(merr, x.Close())
// 		}
// 	}
// 	return merr.NonNilError()
// }

func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// // if a Connection.Close header, just return nothing
	// if r.Header.Get("Connection") == "close" {
	// 	log.Warning(nil, "Connection:close for Host: %s, Method: %v, URL: %v, RequestURI: %v, Headers: %v",
	// 		r.Host, r.Method, r.URL, r.RequestURI, r.Header)

	// 	return
	// }

	log.Debug(nil, "Engine received request: Host: %s, Method: %v, URL: %v, RequestURI: %v, Headers: %v",
		r.Host, r.Method, r.URL, r.RequestURI, r.Header)

	// no need to rlock/runlock, since reload first pauses and waits till zero
	// e.mu.RLock()
	// defer e.mu.RUnlock()
	switch r.URL.Path {
	case "":
		// do nothing - continue to later processing
	case e.ExitPath:
		w.Write([]byte("exiting following request for " + e.ExitPath + "\n"))
		go os.Exit(1)
		return
	case e.PingPath:
		w.Write([]byte("pinging following request for " + e.PingPath + "\n"))
		return
	case e.ReloadPath:
		w.Write([]byte("reloading following request for " + e.ReloadPath + "\n"))
		go e.reload()
		return
	}

	var svr *Server

L1:
	for j, _ := range e.Servers {
		s := &e.Servers[j]
		for _, re := range s.hosts {
			// if r.Host == str {
			if re.MatchString(r.Host) {
				svr = s
				break L1
			}
		}
	}

	if svr == nil {
		log.Error(nil, "No servers found for Host: %s, Method: %v, URL: %v, RequestURI: %v, Headers: %v",
			r.Host, r.Method, r.URL, r.RequestURI, r.Header)
		http.Error(w, "No servers found for Host: "+r.Host, 500)
		return
	}
	if !svr.inited {
		log.Error(nil, "Server Not Initialized. Host: %s, URL: %v, RequestURI: %v, Headers: %v",
			r.Host, r.URL, r.RequestURI, r.Header)
		http.Error(w, "Server Not Initialized", 500)
		return
	}
	svr.ServeHTTP(w, r)
}
