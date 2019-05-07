package simplewebsite

// This is the watcher implementation for other OS'es.
// For those, we are not as precise as on linux.
// We use fsnotify from github.com/howeyc/fsnotify
// which gives macro events CREATE|MODIFY|DELETE
//
// watcher tracks the following events:
//    - dir create, delete, move
//    - file write, delete, move
//
// configFile changes and directory moves always trigger a full reload.
// It's not incremental.
//
// Also, any incremental changes to page files (*.page.*) will recreate
// static site completely. There is no "incremental"
// generation of static site data, because tags, feeds and dir indexes
// can refer to files beyond a single file change.
//
// For these MACRO updates (full reload, createStatic), we
// bundle the updates and do it once at the end of processing a bunch of reads.
//

import (
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

func init() {
	_ = fsnotify.NewWatcher
}

// Only handle pages that start and end with alphanumeric characters.
// This affords cases like when linux creates temporary lock files for open files.
var watchRe = regexp.MustCompile(`^[a-zA-Z0-9_-].*[a-zA-Z0-9_-]$`)

type watcher struct {
	e  *Engine
	ew *fsnotify.Watcher
	fw map[*Server]*fsnotify.Watcher
	ch map[*Server]bool
	mu sync.RWMutex
	ec chan bool
}

func newWatcher(e *Engine) error {
	w := &watcher{
		fw: make(map[*Server]*fsnotify.Watcher, len(e.Servers)),
		ch: make(map[*Server]bool, len(e.Servers)),
		ec: make(chan bool, 4),
		e:  e,
	}
	z, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.ew = z
	e.watcher = w
	w.ew.Add(e.configFile)
	go func() {
		for ev := range w.ew.Events {
			if ev.Op == fsnotify.Write && ev.Name == e.configFile {
				e.reload()
			}
		}
	}()

	tt := time.NewTicker(2 * time.Second)
	go func() {
	L:
		for {
			select {
			case <-w.ec:
				break L
			case <-tt.C:
				// log.Debug(nil, "watch: checking to see if any server reload needed: num servers to reload = %d", len(w.ch))
				w.mu.RLock()
				for k := range w.ch {
					log.Debug(nil, "watch: reloading server with name: %s due to changed file", k.name)
					ww := w.fw[k]
					ww.Close()
					delete(w.fw, k)
					delete(w.ch, k)
					k.reload()
					w.reload(k)
				}
				w.mu.RUnlock()
			}
		}
	}()

	for i := range w.e.Servers {
		s := &w.e.Servers[i]
		if err := w.reload(s); err != nil {
			return err
		}
	}
	return nil
}

func (w *watcher) reload(s *Server) error {
	ww, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.fw[s] = ww

	walkFn := func(fpath string, info os.FileInfo, inerr error) error {
		fpath = filepath.Clean(fpath)
		if info.Mode().IsDir() {
			err := ww.Add(fpath)
			if err == nil {
				log.Debug(nil, "watch: Added path: %s to server: %s", fpath, s.name)
			} else {
				log.IfError(nil, err, "Watch: Error adding path: %s", fpath)
			}
		}
		return nil
	}
	if err := filepath.Walk(filepath.Clean(s.BaseDir), walkFn); err != nil {
		log.IfError(nil, err, "Watch: Error walking server basedir: %s", s.BaseDir)
	}

	go func() {
		for e := range ww.Events {
			base := filepath.Base(e.Name)
			log.Debug(nil, "Found an event: %v, matching: %v", e, watchRe.MatchString(base))
			if !watchRe.MatchString(base) {
				continue
			}
			switch e.Op {
			case fsnotify.Create, fsnotify.Remove, fsnotify.Rename, fsnotify.Write:
				w.mu.Lock()
				w.ch[s] = true
				w.mu.Unlock()
			}
		}
	}()
	return nil
}

func (w *watcher) Close() error {
	w.ew.Close()
	for k, v := range w.fw {
		v.Close()
		delete(w.fw, k)
	}
	w.ec <- true
	return nil
}
