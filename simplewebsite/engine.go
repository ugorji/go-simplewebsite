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
	"github.com/ugorji/go-common/util"
	"github.com/ugorji/go-web"
	// "github.com/ugorji/go-common/zerror"
)

type DynamicPathFn func(s *Server, w http.ResponseWriter, r *http.Request) error

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
	closed       bool
	mu           sync.RWMutex
	configFile   string
	fatalErrChan chan error
	engineState
}

func newEngine(cfgFile string, dyn map[string]DynamicPathFn) (e *Engine, err error) {
	e = new(Engine)
	e.configFile = filepath.Clean(cfgFile)
	e.dynamicFns = dyn
	err = e.reload()
	return
}

func (e *Engine) reload() (err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return
	}

	if e.configFile, err = filepath.Abs(e.configFile); err != nil {
		return
	}
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

	// AccessLogFile must be explicitly configured
	// if ecfg.AccessLogFile == "" {
	// 	ecfg.AccessLogFile = filepath.Join(ecfg.RuntimeDir, "simplewebsite.access.log")
	// }
	if ecfg.PidFile == "" {
		ecfg.PidFile = filepath.Join(ecfg.RuntimeDir, "simplewebsite.pid")
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
		if err = util.WriteFile(ecfg.PidFile, zpid, false); err != nil {
			return
		}
	}

	for i := range e.Servers {
		if err = e.Servers[i].runInit(&e.engineCfg); err != nil {
			return
		}
	}
	logging.Info(nil, "Engine Initialized in %v", time.Since(e.reloadTime))

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
					logging.Error2(nil, zerr, "Error closing old webserver at %s", ecfg0.ListenAddress)
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
			logging.Error2(nil, err, "Error starting watch service")
			return
		}
	}
	// if e.watcher != nil {
	// 	if err = e.watcher.reload(); err != nil {
	// 		return
	// 	}
	// }
	logging.Info(nil, "Engine Now Listening at: %s", e.ListenAddress)
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
// 	var merr zerror.Multi
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
		logging.Error(nil, "No servers found for Host: %s, URL: %v, RequestURI: %v, Headers: %v",
			r.Host, r.URL, r.RequestURI, r.Header)
		http.Error(w, "No servers found for Host: "+r.Host, 500)
		return
	}
	if !svr.inited {
		logging.Error(nil, "Server Not Initialized. Host: %s, URL: %v, RequestURI: %v, Headers: %v",
			r.Host, r.URL, r.RequestURI, r.Header)
		http.Error(w, "Server Not Initialized", 500)
		return
	}
	svr.ServeHTTP(w, r)
}
