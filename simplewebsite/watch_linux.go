//+build linux_ignore

// Ugorji 20180225: This is now disabled, as we have decided to use fsnotify package instead.

package simplewebsite

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
	"syscall"
	"time"

	"github.com/ugorji/go-serverapp/fsnotify"
	"github.com/ugorji/go-common/logging"
	"github.com/ugorji/go-common/errorutil"
)

// Only handle pages that start and end with alphanumeric characters.
// This affords cases like when linux creates temporary lock files for open files.
var watchRe = regexp.MustCompile(`^[a-zA-Z0-9_-].*[a-zA-Z0-9_-]$`)

type watcher struct {
	ps map[string]*Server
	e  *Engine
	w  *fsnotify.Watcher
}

func newWatcher(e *Engine) (err error) {
	bufsize, sysbufsize := 256, 512 // 256 batches, 512 inotify events
	w := &watcher{e: e}
	fw, err := fsnotify.NewWatcher(bufsize, sysbufsize, 1*time.Second, w.handleEventList)
	if err != nil {
		return
	}
	w.w = fw
	e.watcher = w
	err = w.reload()
	return
}

// this is called after each successful engine.reload().
// When called, the watcher will remove all its watches,
// then add watches for config file, and all directories under servers.
func (w *watcher) reload() error {
	log.Debug(nil, "Reloading Watch")
	log.Error2(nil, w.w.Clear(), "Watch: Error clearing")
	log.Error2(nil, w.w.Add(w.e.configFile, 0), "Watch: Error adding config file: %s", w.e.configFile)
	w.ps = make(map[string]*Server)
	for i := range w.e.Servers {
		w.reloadServer(&w.e.Servers[i])
	}
	return nil
}

func (w *watcher) clear(silentErr bool) error {
	log.Error2(nil, w.w.Remove(w.e.configFile), "Watch: Error removing config file: %s", w.e.configFile)
	for i := range w.e.Servers {
		w.clearServer(&w.e.Servers[i], silentErr)
	}
	return nil
}

func (w *watcher) Close() error {
	return w.w.Close()
}

func (w *watcher) clearServer(s *Server, silentErr bool) error {
	for k, v := range w.ps {
		if v == s {
			err := w.w.Remove(k)
			// sometimes, events come after file deleted, so no need spitting out spurious errors
			if err != nil && !silentErr {
				log.Error2(nil, err, "Watch: %s: Error removing path: %s", s.name, k)
			}
			delete(w.ps, k)
		}
	}
	return nil
}

func (w *watcher) reloadServer(s *Server) {
	walkFn := func(fpath string, info os.FileInfo, inerr error) error {
		return w.serverWalkFn(s, fpath, info, inerr)
	}
	if err := filepath.Walk(s.BaseDir, walkFn); err != nil {
		log.Error2(nil, err, "Watch: Error walking server basedir: %s", s.BaseDir)
	}
}

func (w *watcher) serverWalkFn(s *Server, fpath string, info os.FileInfo, inerr error) error {
	if info.Mode().IsDir() {
		// println("Adding watch:", fpath)
		if err := w.w.Add(fpath, 0); err == nil {
			w.ps[fpath] = s
		} else {
			log.Error2(nil, err, "Watch: Error adding path: %s", fpath)
		}
	}
	return nil
}

func (w *watcher) handleEventList(events []*fsnotify.WatchEvent) {
	var pageEvents = make(map[*Server]bool)
	var sReloadEvents = make(map[*Server]bool)
	var fullReload bool
	var err error
	for _, raw := range events {
		fpath := raw.Path
		fname := raw.Name
		s := w.ps[fpath]
		// todo: if s == nil, then this event is already handled.
		// println(">>>>>>> s:", s, ", event:", raw.String())
		if raw.Len == 0 {
			// configFile update OR
			// configFile / watch directory moved OR deleted
			switch {
			case fpath == w.e.configFile:
				switch {
				case raw.Mask&syscall.IN_DELETE_SELF != 0, raw.Mask&syscall.IN_MOVE_SELF != 0:
					log.Debug(nil, "Watch: config file moved/deleted: %s", fpath)
					w.e.fatalErrChan <- errorutil.String("Config File moved/deleted")
				default:
					log.Debug(nil, "Watch: configFile. Reload: %s", fpath)
					fullReload = true
				}
			case raw.Mask&syscall.IN_MOVE_SELF != 0 && s != nil:
				// if s==nil, then event already handled via IN_MOVE_FROM/TO
				log.Debug(nil, "Watch: watched dir moved: %s", fpath)
				w.w.Remove(fpath)
				delete(w.ps, fpath)
				sReloadEvents[s] = true
			case raw.Mask&syscall.IN_DELETE_SELF != 0 && s != nil:
				// if s==nil, then event already handled via IN_DELETE
				log.Debug(nil, "Watch: watched dir deleted: %s", fpath)
				w.w.Remove(fpath)
				delete(w.ps, fpath)
				// sReloadEvents[s] = true
				s.fileWatch(fpath, true, true)
			}
		} else {
			fpath2 := filepath.Join(fpath, fname)
			// a file/directory within a watched directory
			switch {
			case raw.Mask&syscall.IN_ISDIR != 0:
				// a directory
				switch {
				case raw.Mask&syscall.IN_CREATE != 0:
					// may be created from a copy of a diff directory.
					// if so, then it's a non-empty directory and we may as well reload the server.
					sReloadEvents[s] = true
					// s.fileWatch(fpath2, true, false)
					w.w.Add(fpath2, 0)
					w.ps[fpath2] = s
				case raw.Mask&syscall.IN_MOVED_TO != 0:
					w.w.Add(fpath2, 0)
					w.ps[fpath2] = s
					sReloadEvents[s] = true
				case raw.Mask&syscall.IN_MOVED_FROM != 0, raw.Mask&syscall.IN_DELETE != 0:
					w.w.Remove(fpath2)
					delete(w.ps, fpath2)
					sReloadEvents[s] = true
				default:
					// log.Debug(nil, "Watch: dir moved to/from watch dir: %s", fpath)
					sReloadEvents[s] = true
				}
			case raw.Mask&syscall.IN_CLOSE_WRITE != 0,
				raw.Mask&syscall.IN_DELETE != 0,
				raw.Mask&syscall.IN_MOVED_FROM != 0,
				raw.Mask&syscall.IN_MOVED_TO != 0:
				// a file. move, delete or write.
				if watchRe.MatchString(fname) && !sReloadEvents[s] { // don't do incrememtal if we already reloading server
					isDelete := raw.Mask&syscall.IN_CLOSE_WRITE == 0
					log.Debug(nil, "Watch: file. IsDelete: %v, for: %s", isDelete, fpath2)
					s.fileWatch(fpath2, false, isDelete)
					if _, ok := pageEvents[s]; !ok && pageRegexp.MatchString(fpath2) {
						pageEvents[s] = true
					}
				}
			}
		}
	}
	// only regenerate pages if a *.page.* change event found
	if fullReload {
		log.Debug(nil, "Watch: Full Reload Triggered")
		w.clear(true)
		if err = w.e.reload(); err == nil {
			w.reload()
		} else {
			log.Error2(nil, err, "Error reloading engine. All watches cleared.")
		}
	} else {
		// if reloading server, then don't create static files again
		for s, _ := range sReloadEvents {
			// if s == nil { // possible
			// 	continue
			// }
			delete(pageEvents, s)
			log.Debug(nil, "Watch: Reload server: %s", s.name)
			w.clearServer(s, true)
			if err = s.reload(); err != nil {
				log.Error2(nil, err, "Error reloading server: %s. Watches cleared.", s.name)
			}
			w.reloadServer(s)
		}
		for s, _ := range pageEvents {
			// if s == nil {
			// 	continue
			// }
			log.Debug(nil, "Watch: Create Static Files for server: %s", s.name)
			log.Error2(nil, s.createStaticFiles(true), "Error creating static site: %s", s.name)
		}
	}
}
