package simplewebsite

import (
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ugorji/go-common/logging"
)

var log = logging.PkgLogger()

type Runner struct {
	Log logging.Flags
	engineInitCfg
}

func (r *Runner) Flags(flags *flag.FlagSet) {
	r.Log.Flags(flags)
	flags.StringVar(&r.Config, "c", "config.json", "Server Configuration")
	flags.StringVar(&r.BaseDir, "d", "", "Default Base Directory for Pages, Config, etc")
	flags.StringVar(&r.BaseRuntimeDir, "r", "", "Default Base Directory for files created during runtime")
	// flags.BoolVar(&r.Watch, "w", false, "(Deprecated and ignored) Watch/Incremental Reload on changes")
}

// Run will create an engine off the config file and possibly watch it for real-time uploads.
// Users can pass a set of dynamic functions, which are checked for a match
// if a dynamic path is seen and not matching one of tag, feed or message.
func (r *Runner) Run() (err error) {
	names := strings.Split(r.Log.Files, ",")
	if err = logging.BasicInit(names, r.Log.Config); err != nil {
		return
	}

	// runtimeutil.P(">>>>>>>>>>> simplewebsite.Run ...: nil? %v \n", log == nil)
	log.Notice(nil, "Starting up")

	e, err := newEngine(r.engineInitCfg)
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
	// 		log.IfError(nil, err, "Error starting watch service")
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
				log.IfError(nil, err, "Fatal Error - WILL SHUT DOWN!!")
				log.Severe(nil, "SHUTTING DOWN ...")
				e.Close()
				return
			}
		case sig := <-sigChan:
			switch sig {
			case syscall.SIGHUP:
				log.Info(nil, "Signal HUP received: will reopen logs and reload engine")
				log.IfError(nil, logging.Reopen(), "Error reopening logging")
				if zerr := e.reload(); zerr != nil {
					log.IfError(nil, zerr, "Reload Err: %v", zerr)
				}
			case syscall.SIGUSR1:
				log.Info(nil, "Signal USR1 received: will reopen logs")
				log.IfError(nil, logging.Reopen(), "Error reopening logging")
				log.IfError(nil, e.accessLogger.Reopen(), "Error reopening webserver logs")
			case syscall.SIGTERM:
				log.Info(nil, "Signal TERM received: will close engine.")
				if w != nil {
					log.IfError(nil, w.Close(), "Error closing watcher")
				}
				log.IfError(nil, e.Close(), "Error closing engine")
				return
			}
		}
	}

	return
}

func Main(cmdName string, args []string) (err error) {
	flags := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	var r Runner
	r.Flags(flags)
	err = flags.Parse(args)
	if err != nil {
		return
	}
	// clean up r.engineInitCfg
	c := &r.engineInitCfg
	if c.BaseDir == "" {
		c.BaseDir = "."
	}
	if !filepath.IsAbs(c.BaseDir) {
		c.BaseDir, err = filepath.Abs(c.BaseDir)
		if err != nil {
			return
		}
	}
	if c.BaseRuntimeDir == "" {
		c.BaseRuntimeDir = c.BaseDir
	}
	if !filepath.IsAbs(c.BaseRuntimeDir) {
		c.BaseRuntimeDir, err = filepath.Abs(c.BaseRuntimeDir)
		if err != nil {
			return
		}
	}
	if c.Config == "" {
		c.Config = "config.json"
	}
	if !filepath.IsAbs(c.Config) {
		c.Config = filepath.Clean(filepath.Join(c.BaseDir, c.Config))
	}
	return r.Run()
}
