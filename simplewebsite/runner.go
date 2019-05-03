package simplewebsite

import (
	"flag"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ugorji/go-common/logging"
)

var log *logging.Logger // set at init

type Runner struct {
	Config         string
	LogFiles       string
	MinLogLevelStr string
	MinLogLevel    logging.Level
	DynamicPathFns map[string]DynamicPathFn
	Watch          bool // Deprecated
}

func (r *Runner) ParseFlags(args []string) (err error) {
	flags := flag.NewFlagSet("simplewebsite", flag.ContinueOnError)
	flags.StringVar(&r.Config, "c", "config.json", "Server Configuration")
	flags.StringVar(&r.LogFiles, "l", "<stderr>", "Log file")
	flags.StringVar(&r.MinLogLevelStr, "v", "INFO", "Log Level Threshold")
	flags.BoolVar(&r.Watch, "w", false, "(Deprecated and ignored) Watch/Incremental Reload on changes")
	if err = flags.Parse(args); err == nil {
		r.MinLogLevel = logging.ParseLevel(r.MinLogLevelStr)
	}
	return
}

// Run will create an engine off the config file and possibly watch it for real-time uploads.
// Users can pass a set of dynamic functions, which are checked for a match
// if a dynamic path is seen and not matching one of tag, feed or message.
func (r *Runner) Run() (err error) {
	if err = logging.Open(1*time.Second, 16<<10, 0); err != nil {
		return
	}
	names := strings.Split(r.LogFiles, ",")
	for _, n := range names {
		if err = logging.AddHandler(n, logging.NewHandlerWriter(nil, n, logging.Human, nil)); err != nil {
			return
		}
	}
	logging.AddLogger("", r.MinLogLevel, nil, names)
	log = logging.PkgLogger()

	// runtimeutil.P(">>>>>>>>>>> simplewebsite.Run ...: nil? %v \n", log == nil)
	log.Debug(nil, "Starting up")

	e, err := newEngine(r.Config, r.DynamicPathFns)
	if err != nil {
		return
	}
	w := e.watcher

	// var e = &Engine{configFile: r.Config}

	// if err = e.reload(); err != nil {
	// 	return
	// }

	// e.engineState.dynamicFns = r.DynamicPathFns

	// var w *watcher
	// if r.Watch {
	// 	if w, err = newWatcher(e, 256, 512); err != nil { // 256 batches, 512 inotify events
	// 		log.Error2(nil, err, "Error starting watch service")
	// 	}
	// 	if w != nil {
	// 		w.reload()
	// 	}
	// }

	// SIGHUP: reload. SIGTERM: graceful shutdown
	sigChan := make(chan os.Signal, 8)
	signal.Notify(sigChan, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGUSR1)
	for {
		select {
		case err = <-e.fatalErrChan:
			if err != nil {
				log.Error2(nil, err, "Fatal Error - WILL SHUT DOWN!!")
				log.Severe(nil, "SHUTTING DOWN ...")
				e.Close()
				return
			}
		case sig := <-sigChan:
			switch sig {
			case syscall.SIGHUP:
				log.Info(nil, "Signal HUP received: will reopen logs and reload engine")
				log.Error2(nil, logging.Reopen(), "Error reopening logging")
				if zerr := e.reload(); zerr != nil {
					log.Error2(nil, zerr, "Reload Err: %v", zerr)
				}
			case syscall.SIGUSR1:
				log.Info(nil, "Signal USR1 received: will reopen logs")
				log.Error2(nil, logging.Reopen(), "Error reopening logging")
				log.Error2(nil, e.accessLogger.Reopen(), "Error reopening webserver logs")
			case syscall.SIGTERM:
				log.Info(nil, "Signal TERM received: will close engine.")
				if w != nil {
					log.Error2(nil, w.Close(), "Error closing watcher")
				}
				log.Error2(nil, e.Close(), "Error closing engine")
				return
			}
		}
	}

	return
}

func Main(args []string) (err error) {
	var r Runner
	if err = r.ParseFlags(args); err != nil {
		return
	}
	return r.Run()
}
