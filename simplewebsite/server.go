package simplewebsite

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/russross/blackfriday"
	"github.com/ugorji/go-common/feed"
	"github.com/ugorji/go-common/osutil"
	"github.com/ugorji/go-common/pool"
	"github.com/ugorji/go-serverapp/web"
)

// Chrome only shows it nicely if application/xml, not application/atom+xml.
// However, for correctness, we use application/atom+xml
const atomContentType = "application/atom+xml"
const errTag = "simplewebsite: "

// const doContinueIfStaticFileNotExist = true

var pageTimeFmts = [...]string{
	time.RFC3339,
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05",
	"2006-01-02",
}

const mailMsgTmplstr = `
Subject: Message from {{ .R.Host }}

MESSAGE

{{ range $k, $v := .R.Form }}
{{ $k }}
{{ range $v }}\t{{ . }}
{{ end }}
{{ end }}
`

var mailMsgTmpl *template.Template

// culled from https://developer.mozilla.org/en-US/docs/Web/HTML/Block-level_elements#Elements
// HTML 3 block elements: https://www.w3.org/TR/2018/SPSD-html32-20180315/#block
// HTML 4 new elements: http://www.htmlhelp.com/reference/html40/new.html#elements
//
// skip top-level blocks that contain no text e.g. img, object etc
const _html3BlockTagStartRe = `p|div|blockquote|dl|ol|ul|h1|h2|h3|h4|h5|h6|pre|table|center|isindex|form|hr`
const _html4BlockTagStartRe = `fieldset|frame|frameset|iframe|noframes|script|noscript`
const _html5BlockTagStartRe = `section|address|header|footer|dir|menu|nav|main`

const _paraRe = `(?si)<p>(.+?)(<(?:/p|` + _html3BlockTagStartRe + `|` + _html4BlockTagStartRe + `|` + _html5BlockTagStartRe + `)>)\s*`

var (
	fsRegexp   = regexp.MustCompile(`\.[a-zA-Z0-9]{2,4}$`)
	pageRegexp = regexp.MustCompile(`\.page\.(thtml|html|md|json|rtxt|txt)$`)

	// headers may have an ID, if using a TOC. Handle that here.
	pageTitleRe   = regexp.MustCompile(`<[hH][1-6](?:.*?)>(.+?)</[hH][1-6]>`)
	paraParaRe    = regexp.MustCompile(_paraRe)
	paraPara2Re   = regexp.MustCompile(`^` + _paraRe)
	htmlTagOrNlRe = regexp.MustCompile(`(?s)</?[a-zA-Z]+.*?/?>|\n`)
	// pageMetaRe = regexp.MustCompile(`<!--\s*?([0-9TZ:-]{24,28})((?:\s*?,\s*?(?:[a-zA-Z0-9-]+))*)\s*?-->`)
	// may not be first line, in case of TOC, etc.
	pageMetaRe = regexp.MustCompile(`<!--\s*\$meta\s+(.*?)-->`)
	// pageMetaSepRe = regexp.MustCompile(`[\s,]+`)
	pageMeta1Re = regexp.MustCompile(`"[^"]*"|'[^']*'|[^\s,]+`)
	// pageTagRe = regexp.MustCompile(`\s*?,\s*?([a-zA-Z0-9-]+)`)

	// See https://en.wikipedia.org/wiki/Uniform_Resource_Identifier  TODO: fix this
	// URI = scheme:[//authority]path[?query][#fragment] ... where authority = [userinfo@]host[:port]
	uriRe = regexp.MustCompile(`(?i)[\w-.+]+:` + `(?://[\w-:@.\[\]]+/)?` + `(?:[\w-+@&;/]+)` + `(?:\?[\w-+&=;/]*)?` + `(?:#[\w-+&;?=/]*)?`)
)

var (
	markdownExtensions int
	markdownFlags      int
	markdownTOCFlags   int
)

func init() {
	// instead of using blackfriday.MarkdownCommon,
	// configure precisely the markdown configuration I want.
	markdownExtensions = 0 |
		blackfriday.EXTENSION_NO_INTRA_EMPHASIS |
		blackfriday.EXTENSION_TABLES |
		blackfriday.EXTENSION_FENCED_CODE |
		blackfriday.EXTENSION_AUTOLINK |
		blackfriday.EXTENSION_STRIKETHROUGH |
		blackfriday.EXTENSION_SPACE_HEADERS |
		blackfriday.EXTENSION_HEADER_IDS |
		blackfriday.EXTENSION_AUTO_HEADER_IDS |
		0
	markdownFlags = 0 |
		blackfriday.HTML_USE_XHTML |
		blackfriday.HTML_USE_SMARTYPANTS |
		blackfriday.HTML_SMARTYPANTS_FRACTIONS |
		blackfriday.HTML_SMARTYPANTS_LATEX_DASHES |
		0
	markdownTOCFlags = markdownFlags | blackfriday.HTML_TOC

	// markdownRenderer = blackfriday.HtmlRenderer(commonHtmlFlags, "", "")

	var err error
	if mailMsgTmpl, err = template.New("").Parse(mailMsgTmplstr); err != nil {
		panic(err)
	}
}

type User struct {
	FullName string // e.g. FirstName LastName
	Username string // e.g. nickname
	URL      string // e.g. mailto:xxx@yyy.net, http://xxx.net, etc
}

type TagStat struct {
	Tag   string
	Count int
}

type sortedTagStats []TagStat

func (x sortedTagStats) Len() int           { return len(x) }
func (x sortedTagStats) Less(i, j int) bool { return x[i].Count > x[j].Count }
func (x sortedTagStats) Swap(i, j int)      { x[i], x[j] = x[j], x[i] }

// Dir stores a name, and data about the templates loaded from within
// it and the replacements loaded from it.
//
// Figuring out what templates and replacements apply at runtime.
// This allows us to reload files one at a time.
type Dir struct {
	tmpls map[string]*template.Template        // filename to template(s) loaded from it.
	name  string                               // name of dir
	repl  map[string]map[*regexp.Regexp]string // filename to regexp/match loaded
}

type serverCfg struct {
	Hosts                []string // hosts for which this Server should handle requests
	BaseUrl              string
	Users                []*User
	Title                string
	BaseDir              string
	DefaultUser          string // default author
	Forbidden            string
	Permitted            string
	DynamicPathPfx       string // defaults to /d/ if not set
	PageExt              string // must be blank, or .xxxxx (all lower case)
	StaticIndexFile      string
	SMTPAddr             string
	CacheControlMaxHours int
	PrimaryTags          []string
	Rewrites             map[string]string
	PageTOC              bool
}

type Server struct {
	serverCfg

	forbidden      *regexp.Regexp
	permitted      *regexp.Regexp
	rewrites       map[*regexp.Regexp]string
	pagesDir       string
	pagesToHtmlDir string
	runtimeDir     string
	staticDir      string
	sortedPages    []*Page
	pages          map[string]*Page
	dirs           map[string]*Dir
	users          map[string]*User
	tmplFn         template.FuncMap
	mu             sync.RWMutex
	dynamicFns     map[string]DynamicPathFn
	hosts          []*regexp.Regexp
	properties     map[string]interface{}
	name           string
	lastModTime    time.Time
	inited         bool
}

type tmplParam struct {
	Server    *Server
	Path      string
	Error     string
	ErrorCode int
	Tag       string
	TagDir    string
	Page      *Page
	Writer    io.Writer
}

// return a non-initialized clone
func (s *Server) clone() *Server {
	return &Server{
		serverCfg: s.serverCfg,
	}
}

func (s *Server) runInit(es *engineCfg) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// a server is never init'ed twice
	if s.inited {
		return fmt.Errorf("%sServer already initialized", errTag)
	}

	// merge from engine server template.
	// We override zero values, or augment a non-zero value.
	{
		s1 := &es.ServerTemplate
		for _, u1 := range s1.Users {
			var found bool
			for _, u := range s.Users {
				if u.Username == u1.Username {
					found = true
					break
				}
			}
			if !found {
				s.Users = append(s.Users, u1)
			}
		}

		for k1, v1 := range s1.Rewrites {
			if _, ok := s.Rewrites[k1]; !ok {
				if s.Rewrites == nil {
					s.Rewrites = make(map[string]string)
				}
				s.Rewrites[k1] = v1
			}
		}

		if s1.Title != "" && s.Title == "" {
			s.Title = s1.Title
		}
		if s1.DefaultUser != "" && s.DefaultUser == "" {
			s.DefaultUser = s1.DefaultUser
		}
		if s1.Forbidden != "" && s.Forbidden == "" {
			s.Forbidden = s1.Forbidden
		}
		if s1.Permitted != "" && s.Permitted == "" {
			s.Permitted = s1.Permitted
		}
		if s1.DynamicPathPfx != "" && s.DynamicPathPfx == "" {
			s.DynamicPathPfx = s1.DynamicPathPfx
		}
		if s1.PageExt != "" && s.PageExt == "" {
			s.PageExt = s1.PageExt
		}
		if s1.StaticIndexFile != "" && s.StaticIndexFile == "" {
			s.StaticIndexFile = s1.StaticIndexFile
		}
		if s1.CacheControlMaxHours != 0 && s.CacheControlMaxHours == 0 {
			s.CacheControlMaxHours = s1.CacheControlMaxHours
		}
		if len(s1.PrimaryTags) != 0 && len(s.PrimaryTags) == 0 {
			s.PrimaryTags = append(s.PrimaryTags, s1.PrimaryTags...)
		}
	}

	s.lastModTime = time.Now().UTC()
	// if s.lastModTime.IsZero() {
	// 	s.lastModTime = es.reloadTime
	// }

	// s.DynamicPathPfx = strings.TrimFunc(s.DynamicPathPfx, notSlash)
	// dynamic path prefix must be /xxx/
	if s.DynamicPathPfx == "" {
		s.DynamicPathPfx = "/d/"
	}
	if len(s.Hosts) == 0 {
		return fmt.Errorf("%sNo Hosts specified for server: %s", errTag, s.Title)
	}
	s.hosts = make([]*regexp.Regexp, len(s.Hosts))
	for i, str := range s.Hosts {
		s.hosts[i] = regexp.MustCompile(str)
	}
	s.users = make(map[string]*User, len(s.Users))
	for _, v := range s.Users {
		s.users[v.Username] = v
	}
	if s.Forbidden != "" {
		s.forbidden = regexp.MustCompile(s.Forbidden)
	}
	if s.Permitted != "" {
		s.permitted = regexp.MustCompile(s.Permitted)
	}
	if s.BaseDir, err = filepath.Abs(s.BaseDir); err != nil {
		return
	}
	var zb []byte
	zprops := filepath.Join(s.BaseDir, "_properties.json")
	if fi, _ := os.Stat(zprops); fi != nil {
		if zb, err = ioutil.ReadFile(zprops); err != nil {
			return
		}
		if err = json.NewDecoder(bytes.NewBuffer(zb)).Decode(&s.properties); err != nil {
			return
		}
	}
	s.pagesDir = s.BaseDir
	if _, _, err = osutil.ChkDir(s.pagesDir); err != nil {
		return
	}

	url, err := url.ParseRequestURI(s.BaseUrl)
	if err != nil {
		return
	}
	s.name = fmt.Sprintf("%s-%s-%s", url.Hostname(), url.Port(), filepath.Base(s.BaseDir)) //, s.lastModTime.Format("20060102T150405"))

	s.runtimeDir = filepath.Join(es.RuntimeDir, s.name)
	s.pagesToHtmlDir = filepath.Join(s.runtimeDir, "_pages_to_html")
	s.staticDir = filepath.Join(s.runtimeDir, "_static")

	s.tmplFn = template.FuncMap{
		"_title":      toTitle,
		"_fmt_time":   formatTime,
		"_has_prefix": strings.HasPrefix,
	}

	if len(s.Rewrites) > 0 {
		s.rewrites = make(map[*regexp.Regexp]string)
		var re *regexp.Regexp
		for k, v := range s.Rewrites {
			if re, err = regexp.Compile(k); err != nil {
				return
			}
			s.rewrites[re] = v
		}
	}

	// filepath.Base(s.BaseDir) + "-" + base64.URLEncoding.EncodeToString([]byte(s.BaseDir)))

	if err = s.reload(); err != nil {
		return
	}
	s.inited = true
	return
}

func (s *Server) reload() (err error) {
	// Always remove first, to ensure that we don't try to create file from dir, etc
	if fi, _ := os.Stat(s.runtimeDir); fi != nil {
		if err = os.RemoveAll(s.runtimeDir); err != nil {
			return
		}
	}
	if err = osutil.MkDir(s.runtimeDir); err != nil {
		return
	}

	if err = osutil.MkDir(s.pagesToHtmlDir); err != nil {
		return
	}

	if err = osutil.MkDir(s.staticDir); err != nil {
		return
	}
	if len(s.sortedPages) != 0 {
		s.sortedPages = s.sortedPages[:0]
	}
	s.dirs = make(map[string]*Dir)
	s.pages = make(map[string]*Page)

	// s.fsHdlr = http.FileServer(http.Dir(s.BaseDir))
	// s.staticHdlr = http.FileServer(http.Dir(s.staticDir))

	var zwg sync.WaitGroup
	var zpages = make([]*Page, 0, 16)
	var zchan = make(chan *Page, 16)

	go func() {
		for x := range zchan {
			zpages = append(zpages, x)
		}
	}()

	fnWalk := func(fpath string, info os.FileInfo, inerr error) error {
		return s.initWalk(&zwg, zchan, fpath, info, inerr)
	}
	err = filepath.Walk(s.pagesDir, fnWalk)

	zwg.Wait()
	close(zchan)

	if err != nil {
		return
	}

	// Look through all pages, and ensure there's entry in s.sortedPages, and pages
	for _, v := range zpages {
		s.updatePage(v)
	}
	sort.Sort(sortedPagesRecent(s.sortedPages))

	if err = s.createStaticFiles(false); err != nil {
		return
	}
	// fmt.Printf("%v, %v\n", s.dirs, len(s.dirs))
	return
}

func (s *Server) updatePage(v *Page) {
	zp, mok := s.pages[v.name]
	if mok {
		zp.title = v.title
		zp.summary = v.summary
		if len(zp.Tags) == 0 {
			zp.Tags = v.Tags
		}
	} else {
		zp = v
		zp.server = s
		s.pages[zp.name] = zp
		s.sortedPages = append(s.sortedPages, zp)
	}
	s.initPageMeta(zp)
}

func (s *Server) initPageMeta(zp *Page) {
	if len(zp.modTimes) == 0 {
		zp.modTimes = append(zp.modTimes, s.lastModTime)
	}
}

// Load all at startup
//    - template (*.thtml)
//    - page metadata (*.json)
//    - page files (.md, .html)
func (s *Server) initWalk(zwg *sync.WaitGroup, zchan chan *Page,
	fpath string, info os.FileInfo, inerr error) (outerr error) {
	if inerr != nil {
		return inerr
	}

	stop, err := s.fileLoad(zwg, zchan, fpath, info.Mode().IsDir())
	if stop {
		outerr = err
	}
	return
}

func (s *Server) fileLoad(zwg *sync.WaitGroup, zchan chan *Page, fpath string,
	isDir bool) (stop bool, err error) {
	fpath, relpath, err := s.absAndRelPath(fpath)
	if err != nil {
		return
	}
	if isDir {
		if _, ok := s.dirs[relpath]; !ok {
			s.dirs[relpath] = &Dir{name: relpath}
			log.Debug(nil, "%s: Added dir: %s", s.name, fpath)
		}
		return
	}
	if !pageRegexp.MatchString(fpath) {
		// as-is file. copy to static directory.
		if err = osutil.CopyFile(filepath.Join(s.staticDir, relpath), fpath, true); err != nil {
			return
		}
		log.Debug(nil, "%s: Copied to static dir: %s", s.name, fpath)
		return
	}

	filebase := filepath.Base(fpath)

	fnAddPage := func(pageName string, md bool) {
		dest := filepath.Join(s.pagesToHtmlDir, pageName+".html")
		var zp *Page
		var zerr error
		if md {
			zp, zerr = s.pageMarkdownToHtml(dest, fpath, s.findDir(relpath))
		} else {
			zp, zerr = s.pageHtmlToHtml(dest, fpath, s.findDir(relpath))
		}
		if zerr == nil {
			zp.name = pageName
			if zchan != nil {
				zchan <- zp
			} else {
				s.updatePage(zp)
				sort.Sort(sortedPagesRecent(s.sortedPages))
			}
			if md {
				log.Debug(nil, "%s: convert markdown to html: %s", s.name, fpath)
			} else {
				log.Debug(nil, "%s: as-is copy html file: %s", s.name, fpath)
			}
		}
		if zwg != nil {
			zwg.Add(-1)
		}
	}

	switch {
	case strings.HasSuffix(filebase, ".rtxt"):
		err = s.readPageReplacements(fpath)
	case strings.HasSuffix(filebase, ".thtml"):
		err, stop = s.readTemplate(fpath)
	case strings.HasSuffix(filebase, ".json"):
		err = s.readPageMetadata(fpath)
	case strings.HasSuffix(filebase, ".md"):
		if zwg != nil {
			zwg.Add(1)
			go fnAddPage(relpath[:len(relpath)-8], true)
		} else {
			fnAddPage(relpath[:len(relpath)-8], true)
		}
		// s.pageMarkdownToHtml(filepath.Join(s.pagesToHtmlDir, rb + ".html"), fpath)
	case strings.HasSuffix(filebase, ".html"):
		if zwg != nil {
			zwg.Add(1)
			go fnAddPage(relpath[:len(relpath)-10], false)
		} else {
			fnAddPage(relpath[:len(relpath)-10], false)
		}
		// osutil.CopyFiles(filepath.Join(s.pagesToHtmlDir, relpath), fpath)
	}
	return

}

func (s *Server) absAndRelPath(fpath string) (abspath, relpath string, err error) {
	if abspath, err = filepath.Abs(fpath); err == nil {
		relpath, err = relativePath(s.pagesDir, fpath)
	}
	return
}

func (s *Server) fileWatch(fpath string, isDir, isDelete bool) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if isDelete || !isDir {
		if err = s.fileWatchDelete(fpath, isDir); err != nil {
			return
		}
	}
	if !isDelete {
		if _, err = s.fileLoad(nil, nil, fpath, isDir); err != nil {
			return
		}
	}
	// fileWatcher service handles calling createStaticFiles if necessary (as a macro event)
	// // only regenerate pages if a *.page.* change
	// if pageRegexp.MatchString(fpath) {
	// 	err = s.createStaticFiles()
	// }

	s.lastModTime = time.Now().UTC()
	return
}

// Beginnings of support for incremental update.
// NOTE: Only file paths can come in here, not directories.
func (s *Server) fileWatchDelete(fpath string, isDir bool) (err error) {
	// if file matches static file
	// if file matches
	fpath, relpath, err := s.absAndRelPath(fpath)
	if err != nil {
		return
	}
	// if dir, remove it from s.dirs
	if isDir {
		delete(s.dirs, relpath)
		return
	}

	// if not page, remove from static dir, and remove from _pages_to_html
	if !pageRegexp.MatchString(fpath) {
		os.Remove(filepath.Join(s.staticDir, relpath))
		return
	}

	fnRmPage := func(s *Server, pageName string) {
		dest := filepath.Join(s.pagesToHtmlDir, pageName+".html")
		os.Remove(dest)
		os.Remove(dest + ".gz")
		// remove from s.pages
		delete(s.pages, pageName)
		// remove from s.sortedPages
		for i, v := range s.sortedPages {
			if v.name == pageName {
				// s.sortedPages = append(s.sortedPages[:i], s.sortedPages[i+1:]...)
				copy(s.sortedPages[i:], s.sortedPages[i+1:])
				s.sortedPages = s.sortedPages[:len(s.sortedPages)-1]
				break
			}
		}
	}

	reldir := filepath.Dir(relpath)
	if reldir == "." {
		reldir = ""
	}
	d := s.dirs[reldir]
	filebase := filepath.Base(fpath)

	// println(">>>>> relpath:", relpath, ", dir:", reldir, ", d:", d)
	switch {
	case strings.HasSuffix(filebase, ".rtxt"):
		delete(d.repl, filebase)
	case strings.HasSuffix(filebase, ".thtml"):
		delete(d.tmpls, filebase)
	case strings.HasSuffix(filebase, ".json"):
		// do nothing, since metadata could have come from file also
	case strings.HasSuffix(filebase, ".md"):
		fnRmPage(s, relpath[:len(relpath)-8])
	case strings.HasSuffix(filebase, ".html"):
		fnRmPage(s, relpath[:len(relpath)-10])
	}
	return
}

func (s *Server) createStaticFiles(lock bool) (err error) {
	if lock {
		s.mu.Lock()
		defer s.mu.Unlock()
	}

	log.Debug(nil, "%s: Creating static files", s.name)

	var num int

	gwPool := web.NewGzipWriterPool(gzip.BestCompression, 4, len(s.sortedPages)/2)
	t0 := time.Now()
	defer func() {
		log.Info(nil, "%s: Created %d static file pairs (regular and gzip), in %v, with error: %v",
			s.name, num, time.Since(t0), err)
		gwPool.Drain()
	}()

	// go thru dirs and create static index files (if _dir template exists)
	var buf = new(bytes.Buffer)
	tp := &tmplParam{Server: s, Writer: buf}

	dirs := make([]string, 0, len(s.dirs))
	for _, dir := range s.dirs {
		dirs = append(dirs, dir.name)
	}
	log.Debug(nil, "%s: Static Site dirs: %v", s.name, dirs)
	for i, dir := range dirs {
		tp.Path = dir + "/"
		if t, _ := s.findTemplate(tp.Path, "_dir"); t != nil {
			buf.Reset()
			if err = t.Execute(buf, tp); err != nil {
				return
			}
			err = writeStaticFile(filepath.Join(s.staticDir, dirs[i], s.StaticIndexFile), buf.Bytes(), true, gwPool)
			if err != nil {
				return
			}
			num++
		}
	}
	// fmt.Printf("@@@@@@@@@@ Num pages for dirs: %d\n", num)

	// go thru pages and create page views
	for _, p := range s.sortedPages {
		tp.Path = p.name
		tp.Page = p
		if t, _ := s.findTemplate(tp.Path, "_page"); t != nil {
			buf.Reset()
			if err = t.Execute(buf, tp); err != nil {
				return
			}
			if err = writeStaticFile(filepath.Join(s.staticDir, p.name+s.PageExt),
				buf.Bytes(), true, gwPool); err != nil {
				return
			}
			num++
		}
	}
	// fmt.Printf("@@@@@@@@@@ Num pages for sortedPages: %d\n", num)
	tp.Page = nil
	// gather all tags
	allTags := make(map[string]struct{})
	for _, p := range s.sortedPages {
		for _, t := range p.Tags {
			if _, ok := allTags[t]; !ok {
				allTags[t] = struct{}{}
			}
		}
	}

	// for each tag/folder combo, create rss and tag-list views
	tp.Path = "/"
	t, err := s.findTemplate(tp.Path, "_tag")
	if err != nil {
		return
	}
	for i, dir := range dirs {
		// println(">>>>>>> dir:", dir)
		for tag, _ := range allTags {
			buf.Reset()
			s.AtomFeed(s.BaseUrl, dirs[i], tag, -1).ToAtom(buf, "    ")
			if err = writeStaticFile(
				filepath.Join(s.staticDir, s.DynamicPathPfx+"feed/"+tag+"/"+dir+"/"+s.StaticIndexFile),
				buf.Bytes(), true, gwPool); err != nil {
				return
			}
			num++
			buf.Reset()
			tp.Tag, tp.TagDir = tag, dirs[i]
			if err = t.Execute(buf, tp); err != nil {
				return
			}
			if err = writeStaticFile(
				filepath.Join(s.staticDir, s.DynamicPathPfx+"tag/"+tag+"/"+dir+"/"+s.StaticIndexFile),
				buf.Bytes(), true, gwPool); err != nil {
				return
			}
			num++
		}
	}
	// fmt.Printf("@@@@@@@@@@ Num pages at end: %d\n", num)
	return
}

func (s *Server) readTemplate(fpath string) (err error, stop bool) {
	var zb []byte
	dir, err := relativePath(s.pagesDir, filepath.Dir(fpath))
	if err != nil {
		return
	}
	if zb, err = ioutil.ReadFile(fpath); err != nil {
		return
	}
	z := template.New("")
	z.Funcs(s.tmplFn)

	if _, err = z.Parse(string(zb)); err != nil {
		// errors parsing templates should fail startup/reload
		log.Error(nil, "error parsing: %s", fpath)
		stop = true
		return
	}
	d := s.dirs[dir]
	if d.tmpls == nil {
		d.tmpls = make(map[string]*template.Template)
	}
	d.tmpls[filepath.Base(fpath)] = z
	log.Debug(nil, "%s: Parsed template: tmplpath: %s, from: %s", s.name, dir, fpath)
	return
}

func (s *Server) readPageMetadata(fpath string) (err error) {
	var zb []byte
	var relpath string
	if relpath, err = relativePath(s.pagesDir, fpath); err != nil {
		return
	}
	zb, err = ioutil.ReadFile(fpath)
	if err != nil {
		return
	}
	var zs = struct {
		Timestamp string
		Tags      []string
		Username  string
	}{}
	err = json.NewDecoder(bytes.NewBuffer(zb)).Decode(&zs)
	if err != nil {
		return
	}
	var zp = Page{server: s, Username: zs.Username, Tags: zs.Tags}
	if len(zs.Timestamp) > 2 {
		if tt1, err1 := time.Parse(time.RFC3339, zs.Timestamp); err1 != nil {
			zp.modTimes = append(zp.modTimes, tt1)
		}
	}
	zp.name = relpath[:len(relpath)-10]
	// don't set Title. Instead, depend on Title function.
	// if zp.Title == "" {
	// 	zp.Title = toTitle(zp.name)
	// }
	s.initPageMeta(&zp)
	s.pages[zp.name] = &zp
	s.sortedPages = append(s.sortedPages, &zp)
	log.Debug(nil, "%s: Loaded metadata for page: %s, from: %s", s.name, zp.name, fpath)
	return
}

// readPageReplacements expects files in the format:
// ---------BOUNDARY----------------
// regexp
// replace line
// replace
// ---------BOUNDARY----------------
//
// ie
//   - first line of file is the boundary:
//     [boundary][NL]
//   - after that, expect records in form:
//     [regexp][NL][replacement][NL][boundary][NL]
//   - replacement can be multiple lines.
func (s *Server) readPageReplacements(fpath string) (err error) {
	var zb []byte
	dir, err := relativePath(s.pagesDir, filepath.Dir(fpath))
	if err != nil {
		return
	}
	if zb, err = ioutil.ReadFile(fpath); err != nil {
		return
	}
	i := bytes.IndexByte(zb, '\n')
	if i == -1 {
		err = fmt.Errorf("%sNo boundary in page replacement", errTag)
		return
	}
	boundary := regexp.QuoteMeta(string(zb[0:i]))
	// println(">>>> boundary:", boundary)
	zb = zb[i+1:]
	re, err := regexp.Compile("(.+)\\n(?s:(.*?))\\n" + boundary + "\\n")
	if err != nil {
		return
	}
	m := make(map[*regexp.Regexp]string)
	for _, bs := range re.FindAllSubmatch(zb, -1) {
		// println(">>>> len(bs):", len(bs), "-", bs)
		if re, err = regexp.Compile(string(bs[1])); err != nil {
			return
		}
		m[re] = string(bs[2])
	}
	d := s.dirs[dir]
	if d.repl == nil {
		d.repl = make(map[string]map[*regexp.Regexp]string)
	}
	d.repl[filepath.Base(fpath)] = m
	return
}

func (s *Server) parent(dir *Dir) (p *Dir) {
	if dir.name != "" {
		i := strings.LastIndex(dir.name, "/")
		if i == -1 {
			p = s.dirs[""]
		} else {
			p = s.dirs[dir.name[:i]]
		}
	}
	return p
}

func (s *Server) replacements(dir *Dir) map[*regexp.Regexp]string {
	m := make(map[*regexp.Regexp]string)
	m2 := make(map[string]struct{})
	s.replacements1(dir, m, m2)
	return m
}

func (s *Server) replacements1(dir *Dir, m map[*regexp.Regexp]string, m2 map[string]struct{}) {
	// println(">>>> 0. replacements:", dir)
	if dir == nil {
		return
	}
	// println(">>>> replacements:", dir.name)
	for _, v0 := range dir.repl {
		for k, v := range v0 {
			if _, ok := m2[k.String()]; !ok {
				m2[k.String()] = struct{}{}
				m[k] = v
			}
		}
	}
	s.replacements1(s.parent(dir), m, m2)
}

func (s *Server) template(dir *Dir) *template.Template {
	// this template creation NOT a problem. takes 32us -> 150us (avg 50us. once each request)
	// t0 := time.Now()
	// defer func() { log.Debug(nil, "Template took: %v", time.Since(t0)) }()
	// println(">>> Calling s.Template")
	t := template.New("")
	t.Funcs(s.tmplFn)
	s.template1(t, dir, true)
	return t
}

func (s *Server) template1(t *template.Template, dir *Dir, isTop bool) {
	if dir == nil {
		return
	}
	for _, v := range dir.tmpls {
		for _, v2 := range v.Templates() {
			if isTop || v2.Name() != "_dir" {
				if _, err := t.AddParseTree(v2.Name(), v2.Tree); err == nil {
					// println(">>>>> Added Template:", v2.Name())
				}
			}
		}
	}
	s.template1(t, s.parent(dir), false)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Sequence:
	//   - check if a redirect is needed (rewrite)
	//   - cursory determination if a dynamic or static path
	//   - do the following in sequence (break once done)
	//     - if cursory static path
	//       - if trailing slash wrong, redirect appropriately
	//       - if exist, serve
	//     - try to serve as dynamic file
	//     - try to serve attachment (file with extension)
	//     - try to load _dir (dir template)
	//     - try to load _page (page template)
	//     - try to load page

	s.mu.RLock()
	defer s.mu.RUnlock()

	// println(">url.path: ", r.URL.Path)
	p := r.URL.Path
	var err error
	tp := tmplParam{Server: s, Path: p, Writer: w}
	// if len(p) == 0 || (len(p) == 2 && p[0] == '/' && (p[1] == '_' || p[1] == '.')) {
	if s.forbidden != nil && s.forbidden.MatchString(p) &&
		(s.permitted == nil || !s.permitted.MatchString(p)) {
		err = fmt.Errorf("%sInvalid/Forbidden Path: %s%s", errTag, r.Host, p)
		tp.Error, tp.ErrorCode = err.Error(), 404
		s.sendError(&tp)
		return
	}

	zurl := r.Host + r.URL.Path
	// fmt.Printf("zurl: %s\n", zurl)
	for k, v := range s.rewrites {
		// fmt.Printf("Checking rewrite: template: %v\n", v)
		// if idx := k.FindStringSubmatchIndex(zurl); idx != nil {
		var bs []byte
		pt := 0
		for _, idx := range k.FindAllStringSubmatchIndex(zurl, -1) {
			bs = append(bs, zurl[pt:idx[0]]...)
			bs = k.ExpandString(bs, v, zurl, idx)
			pt = idx[1]
		}
		if bs != nil {
			bs = append(bs, zurl[pt:]...)
			http.Redirect(w, r, string(bs), 301)
			return
		}
	}

	isDynPath := len(p) >= len(s.DynamicPathPfx) && p[:len(s.DynamicPathPfx)] == s.DynamicPathPfx
	var dynPath string
	if isDynPath {
		dynPath = p[len(s.DynamicPathPfx):]
	}

	var sortaStatic bool
	if r.Method == "GET" {
		if !isDynPath || strings.HasPrefix(dynPath, "tag") || strings.HasPrefix(dynPath, "feed") {
			sortaStatic = true
		}
	}

	// Check if a conditional GET, and if so, return 304.
	// We considered using the more recent of template or page timestamps.
	// However, this brought un-necessary complications, because templates may change
	// but page remains same, or timestamp in page may not reflect minor updates.
	// Safest to always use lastModTime of server as last update time.
	zmodtime := s.lastModTime
	var sendNotModified bool
	if sortaStatic {
		var zt time.Time
		if zs := r.Header.Get("If-Modified-Since"); zs != "" {
			zt, _ = time.Parse(http.TimeFormat, zs)
		}
		if !zt.IsZero() && zmodtime.Before(zt.Add(5*time.Second)) {
			sendNotModified = true
		}
	}

	// per RFC7273, we need to set same headers for 200 and 304
	// add last-modified header and cache-control (1 week max age)
	w.Header().Set("Last-Modified", zmodtime.Format(http.TimeFormat))
	if s.CacheControlMaxHours > 0 {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", s.CacheControlMaxHours*60*60))
	}

	if sendNotModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// maybe serve static site.
	acceptsGzip := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
	var fi os.FileInfo
	if sortaStatic {
		p2 := filepath.Join(s.staticDir, p)
		log.Debug(nil, "%s: attempt to serve static file for path: %s ==> %s", s.name, p, p2)
		hasSlash := len(p) > 0 && p[len(p)-1] == '/'
		p3 := p2
		if hasSlash {
			p3 = p2[:len(p2)-1]
		}
		fi, err = os.Stat(p3)
		if err == nil {
			if hasSlash {
				if fi.IsDir() {
					p3 = filepath.Join(p3, "/", s.StaticIndexFile)
					fi, err = os.Stat(p3)
					if err == nil {
						s.handleServeFileErr(&tp, s.serveFile(w, p3, acceptsGzip))
						return
					}
				} else {
					log.Debug(nil, "%s: http redirecting to %s (no trailing slash)", s.name, p[:len(p)-1])
					http.Redirect(w, r, p[:len(p)-1], 301)
					return
				}
			} else {
				if fi.IsDir() {
					p3 = filepath.Join(p3, "/", s.StaticIndexFile)
					fi, err = os.Stat(p3)
					if err == nil {
						log.Debug(nil, "%s: http redirecting to %s/ (add trailing slash)", s.name, p)
						http.Redirect(w, r, p+"/", 301)
						return
					}
				} else {
					s.handleServeFileErr(&tp, s.serveFile(w, p3, acceptsGzip))
					return
				}
			}
		}
		log.Debug(nil, "%s: FileNotExist in static directory", s.name)
	}

	if isDynPath {
		p = dynPath
		tp.Path = "/"
		switch {
		case strings.HasPrefix(p, "tag"):
			tp.Tag, tp.TagDir = tagAndDir(p[4:])
			err = s.findExecTmpl(&tp, "_tag", true)
		case strings.HasPrefix(p, "feed"):
			tp.Tag, tp.TagDir = tagAndDir(p[5:])
			w.Header().Set("Content-Type", atomContentType)
			s.AtomFeed("http://"+r.Host, tp.TagDir, tp.Tag, -1).ToAtom(w, "    ")
		case strings.HasPrefix(p, "message"):
			// return 200 as success, or 500 as failure (with body containing error).
			// expect this is typically run via ajax
			// s.handleServeFileErr(&tp, s.mailMessage(r))
			if err = s.mailMessage(r); err != nil {
				s.sendError2(w, 500, err.Error())
			}
		default:
			var done bool
			for k, v := range s.dynamicFns {
				if strings.HasPrefix(p, k) && v != nil {
					err = v(s, w, r)
					done = true
					break
				}
			}
			switch {
			case done && err != nil:
				tp.Error, tp.ErrorCode = err.Error(), 500
				s.sendError(&tp)
			case !done:
				err = fmt.Errorf("%sUnrecognized dynamic request: %s%s", errTag, r.Host, r.URL.Path)
				tp.Error, tp.ErrorCode = err.Error(), 404
				s.sendError(&tp)
			}
		}
		return
	}

	// if non static site, try to serve files with extensions (images, etc)
	if (s.PageExt == "" || !strings.HasSuffix(p, s.PageExt)) &&
		fsRegexp.MatchString(p) &&
		!pageRegexp.MatchString(p) {
		// log.Debug(nil, "Matched static file")
		s.handleServeFileErr(&tp, s.serveFile(w, filepath.Join(s.BaseDir, p), acceptsGzip))
		// s.fsHdlr.ServeHTTP(w, r)
		return
	}

	if p[len(p)-1] == '/' {
		err = s.findExecTmpl(&tp, "_dir", true)
		return
	}

	p1 := p[1:]

	// If serving a page, ensure that the page exists first

	p2 := p1
	if s.PageExt != "" && strings.HasSuffix(p1, s.PageExt) {
		p2 = p1[:len(p1)-len(s.PageExt)]
	}
	var ok bool
	if tp.Page, ok = s.pages[p2]; ok {
		err = s.findExecTmpl(&tp, "_page", true)
		return
	}

	// If page does not exist, check if the path matches a directory
	for _, dir := range s.dirs {
		if dir.name == p1 {
			tp.Path = tp.Path + "/"
			err = s.findExecTmpl(&tp, "_dir", true)
			return
		}
	}

	// All fails, then just send an error
	tp.Error = "Page does not exist: " + p1
	tp.ErrorCode = 404
	s.sendError(&tp)
	return
}

// Need to implement serveFile directly and not use http.FileServer, because:
//   - We shouldn't serve files if StaticIndexFile is not there
//   - Our index file may not be called index.html
//   - we should handle serving gzip version if there
func (s *Server) serveFile(w http.ResponseWriter, fpath string, acceptsGzip bool) (err error) {
	var fi os.FileInfo
	// Browsers do not load css files except content type is correctly defined as text/css.
	// This causes issues with BootStrap.
	// We consequently check and explicitly write the content type ourselves.
	fext := filepath.Ext(fpath)
	// println("fpath: ", fpath, ", ext: '"+fext+"'")
	switch fext {
	case ".css":
		w.Header().Set("Content-Type", "text/css")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript")
	case ".json":
		w.Header().Set("Content-Type", "application/json")
	}

	// check for, and serve gzip file if exist
	if acceptsGzip {
		fpath2 := fpath + ".gz"
		fi, err = os.Stat(fpath2)
		if err == nil && fi != nil && fi.Mode().IsRegular() {
			// try to read first 64 bytes from fpath
			var f *os.File
			if f, err = os.Open(fpath); err == nil {
				defer f.Close()
				bs := make([]byte, web.MinMimeSniffLen)
				var num int
				if num, err = f.Read(bs); err == nil && num >= 16 {
					ctype := w.Header().Get("Content-Type")
					if ctype == "" {
						ctype = http.DetectContentType(bs[:num])
						w.Header().Set("Content-Type", ctype)
					}
					log.Debug(nil, "%s: Sending gzip file: ctype: %s, path: %s",
						s.name, ctype, fpath2)
					w.Header().Set("Content-Encoding", "gzip")
					err = osutil.CopyFileToWriter(w, fpath2)
					return
				}
			}
		}
	}
	err = osutil.CopyFileToWriter(w, fpath)
	return
}

func (s *Server) handleServeFileErr(tp *tmplParam, err error) {
	if err == nil {
		return
	}
	if tp.ErrorCode == 0 {
		if os.IsNotExist(err) {
			tp.ErrorCode = 404
		} else {
			tp.ErrorCode = 500
		}
	}
	tp.Error = err.Error()
	s.sendError(tp)
}

// Find the directory for this path.
// If the path is a directory, it must end with /, else it's a file.
func (s *Server) findDir(path string) *Dir {
	if i := strings.LastIndex(path, "/"); i != -1 {
		path = path[:i]
	}
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	return s.dirs[path]
}

// Find the template by this name under this path.
// If this path doesn't exist, then look under root.
// Expects path for directories to end with /, else they are files.
func (s *Server) findTemplate(path, name string) (t *template.Template, err error) {
	d := s.findDir(path)
	if d == nil {
		d = s.findDir("")
	}
	t = s.template(d)
	if t != nil {
		t = t.Lookup(name)
	}
	if t == nil {
		err = fmt.Errorf("%sNo template found for name: %s, path: %s in server: %s",
			errTag, name, path, s.Title)
		// log.Debug(nil, "Find Template Error: %v", err)
	}
	return
}

func (s *Server) findExecTmpl(p *tmplParam, name string, sendError bool) (err error) {
	// template execution is slow. taking about 2ms on average.
	// t0 := time.Now()
	t, err := s.findTemplate(p.Path, name)
	if err == nil {
		err = t.Execute(p.Writer, p)
	}
	if err != nil && sendError {
		p.Error, p.ErrorCode = err.Error(), 404
		s.sendError(p)
	}
	// log.Debug(nil, "Time to run branch: %s: %v", tp.Path, time.Since(t0))
	return
}

func (s *Server) sendError2(w http.ResponseWriter, code int, message string, params ...interface{}) {
	if wr, _ := w.(web.ResponseWriter); wr != nil && wr.IsHeaderWritten() {
		return
	}
	if w != nil {
		w.Header().Del("Last-Modified")
		w.Header().Del("Cache-Control")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.Error(w, fmt.Sprintf(message, params...), code)
}

func (s *Server) sendError(p *tmplParam) {
	log.Error(nil, "[%d] %v (%s)", p.ErrorCode, p.Error, p.Path)
	wr, _ := p.Writer.(web.ResponseWriter)
	if wr != nil && wr.IsHeaderWritten() {
		return
	}
	t, _ := s.findTemplate(p.Path, "_error")
	// log.Debug(nil, "sendError: template: %v", t)
	hw, _ := p.Writer.(http.ResponseWriter)
	if hw != nil {
		hw.Header().Del("Last-Modified")
		hw.Header().Del("Cache-Control")
	}
	if t != nil {
		if hw != nil {
			hw.Header().Set("Content-Type", "text/html; charset=utf-8")
			hw.WriteHeader(p.ErrorCode)
		}
		if t.Execute(p.Writer, p) == nil {
			return
		}
		if wr != nil && wr.IsHeaderWritten() {
			return
		}
	}
	if hw != nil {
		hw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(hw, p.Error, p.ErrorCode)
	}
}

func (s *Server) SendPageContent(w io.Writer, pageName string) string {
	fpath := filepath.Join(s.pagesToHtmlDir, pageName+".html")
	log.Debug(nil, "%s: SendPageContent: pageName: %s, fpath: %s", s.name, pageName, fpath)
	osutil.CopyFileToWriter(w, fpath)
	return ""
}

func (s *Server) TagStats(namePrefix string) []TagStat {
	var z []TagStat
	for _, p := range s.sortedPages {
		if namePrefix != "" && !strings.HasPrefix(p.name, namePrefix) {
			continue
		}
		for _, t := range p.Tags {
			if t == "" { //defensive. not required.
				continue
			}
			var foundTag bool
			for i := range z {
				if z[i].Tag == t {
					foundTag = true
					z[i].Count++
					break
				}
			}
			if !foundTag {
				z = append(z, TagStat{Tag: t, Count: 1})
			}
		}
	}
	sort.Sort(sortedTagStats(z))
	return z
}

func (s *Server) AtomFeed(schemeAndHost, namePrefix, tag string, limit int) (f *feed.Feed) {
	pages := s.Pages(namePrefix, tag, limit)
	return s.atomFeed(pages, schemeAndHost, namePrefix, tag)
}

func (s *Server) atomFeed(pages []*Page, schemeAndHost, namePrefix, tag string) (f *feed.Feed) {
	f = &feed.Feed{
		Title:    s.Title + ": " + toTitle(tag) + " pages",
		PubDate:  s.lastModTime,
		LastMod:  s.lastModTime,
		SelfLink: schemeAndHost + s.DynamicPathPfx + "feed/" + tag,
		Link:     schemeAndHost + s.DynamicPathPfx + "tag/" + tag,
	}
	if u := s.User(s.DefaultUser); u != nil && u.FullName != "" {
		f.Author = u.FullName
	}
	if namePrefix != "" {
		f.Title += " under '" + namePrefix + "'"
		f.Link += "/" + namePrefix
		f.SelfLink += "/" + namePrefix
	}
	f.Entries = make([]*feed.Entry, len(pages))
	for i, p := range pages {
		f.Entries[i] = &feed.Entry{
			Title:    p.Title(),
			Link:     schemeAndHost + "/" + p.name,
			Desc:     p.summary.String(),
			DescType: feed.TextPlain,
			PubDate:  p.CreateTime(),
			LastMod:  p.LastModTime(),
		}
		if p.Username != "" {
			if u := s.User(p.Username); u != nil && u.FullName != "" {
				f.Entries[i].Author = u.FullName
			}
		}
	}
	return
}

func (s *Server) User(name string) (u *User) {
	return s.users[name]
}

func (s *Server) Pages(namePrefix, tag string, limit int) []*Page {
	if limit < 0 {
		limit = math.MaxInt32
	}
	if tag == "" && namePrefix == "" {
		if limit >= len(s.sortedPages) {
			return s.sortedPages
		}
		return s.sortedPages[:limit]
	}
	var z []*Page
	for _, p := range s.sortedPages {
		if limit <= len(z) {
			return z
		}
		if tag == "" {
			if namePrefix == "" || strings.HasPrefix(p.name, namePrefix) {
				z = append(z, p)
			}
			continue
		}
		for _, t := range p.Tags {
			if limit <= len(z) {
				return z
			}
			if t == tag {
				if namePrefix == "" || strings.HasPrefix(p.name, namePrefix) {
					z = append(z, p)
				}
				break
			}
		}
	}
	return z
}

func (s *Server) mailMessage(r *http.Request) (err error) {
	// parse multipart, and all post and get args
	// gather post and get args in the email text
	// attach multipart args to the email message
	// send the email message

	// Note that email and http multipart are basically the same format.
	// Thus, it should be easy to email directly from the http Body,
	// without calling ParseMultipart first.

	// For now, assume that postfix listens on 25, with no authentication.
	// Only supports sending mail from localhost
	err = r.ParseForm()
	if err != nil {
		return
	}
	// var attachments = make(map[string][]byte)
	type tmplP struct {
		R *http.Request
		M string
	}
	var buf bytes.Buffer
	if err = mailMsgTmpl.Execute(&buf, &tmplP{R: r}); err != nil {
		return
	}
	err = smtp.SendMail(s.SMTPAddr, nil, r.Form.Get("x-from"), r.Form["x-to"], buf.Bytes())
	return
}

func (s *Server) pageMarkdownToHtml(htmlPath, mdPath string, sdir *Dir) (p *Page, err error) {
	zb, err := ioutil.ReadFile(mdPath)
	if err != nil {
		return
	}
	// zb = blackfriday.MarkdownCommon(zb)
	// We cannot share a HtmlRenderer across calls, because it messes up things (errors, etc)
	var r blackfriday.Renderer
	if s.PageTOC {
		r = blackfriday.HtmlRenderer(markdownTOCFlags, "", "")
	} else {
		r = blackfriday.HtmlRenderer(markdownFlags, "", "")
	}
	zb = blackfriday.Markdown(zb, r, markdownExtensions)
	return s.pageBytesToHtml(htmlPath, zb, sdir)
}

func (s *Server) pageHtmlToHtml(htmlPath, srcHtmlPath string, sdir *Dir) (p *Page, err error) {
	// zerr = osutil.CopyFiles(htmlPath, srcHtmlPath, true)
	zb, err := ioutil.ReadFile(srcHtmlPath)
	if err != nil {
		return
	}
	return s.pageBytesToHtml(htmlPath, zb, sdir)
}

func (s *Server) pageBytesToHtml(htmlPath string, zb []byte, sdir *Dir) (p *Page, err error) {
	zbn := zb
	repl := []byte{' '}
	p = new(Page)

	// first do short code replacements, for all dirs
	for k, v := range s.replacements(sdir) {
		var bs []byte
		bv := []byte(v)
		pt := 0
		for _, idx := range k.FindAllSubmatchIndex(zb, -1) {
			bs = append(bs, zb[pt:idx[0]]...)
			bs = k.Expand(bs, bv, zb, idx)
			pt = idx[1]
		}
		if bs != nil {
			bs = append(bs, zb[pt:]...)
			zb = bs
		}
	}

	// We have to use FindXXXIndex here, so we can truncate zbn each time a match is found.
	q := pageMetaRe.FindSubmatchIndex(zbn)
	if len(q) > 2 {
		// allstr := pageMetaSepRe.Split(string(zbn[q[2]:q[3]]), -1)
		allstr := pageMeta1Re.FindAllString(string(zbn[q[2]:q[3]]), -1)
		for _, str := range allstr {
			if len(str) > 0 && (str[0] == '"' || str[0] == '\'') {
				str = str[1 : len(str)-1]
			}
			// if strings.HasSuffix(htmlPath, "test.html") {
			// 	log.Debug(nil, "test.page.md pagemetas: %d: %v", i, str)
			// }
			if str == "" || str == "-" {
				continue
			}

			// if it matches YYYY-, then attempt to parse as a date and add to modtimes.
			if len(str) > 5 && str[4] == '-' &&
				str[0] >= '0' && str[0] <= '9' &&
				str[1] >= '0' && str[1] <= '9' &&
				str[2] >= '0' && str[2] <= '9' &&
				str[3] >= '0' && str[3] <= '9' {
				if tt1 := parsePageTime(str); !tt1.IsZero() {
					p.modTimes = append(p.modTimes, tt1)
					continue
				}
			}

			// it is either the username, or is it an option (starts with :) or a tag
			if str[0] == ':' {
				var s1, s2 string
				s1 = str[1:]
				if ix := strings.IndexByte(s1, ':'); ix >= 0 {
					s2 = s1[ix+1:]
					s1 = s1[:ix]
				}
				switch s1 {
				case "toc":
					p.toc = true
				case "u":
					p.Username = s2
				}
				continue
			}

			p.Tags = append(p.Tags, str)
		}
		zbn = zbn[q[1]:]
	}

	q = pageTitleRe.FindSubmatchIndex(zbn)
	// fmt.Printf("@@@@@@@@@@@ pageTitleRe: FindSubmatchIndex: %s, len: %v: %v\n", htmlPath, len(q), q)
	if len(q) > 2 {
		p.title = html.UnescapeString(string(htmlTagOrNlRe.ReplaceAll(zbn[q[2]:q[3]], repl)))
		zbn = zbn[q[1]:]
	}

	q = paraParaRe.FindSubmatchIndex(zbn)
	if len(q) > 2 {
		p.summary[0] = html.UnescapeString(string(htmlTagOrNlRe.ReplaceAll(zbn[q[2]:q[3]], repl)))
		closeTag := zbn[q[4]:q[5]]
		if len(closeTag) > 3 && closeTag[1] == '/' { // we had a </p> end - trim it out
			zbn = zbn[q[1]:]
		} else {
			zbn = zbn[q[3]:]
		}
		q = paraPara2Re.FindSubmatchIndex(zbn)
		if len(q) > 2 {
			p.summary[1] = html.UnescapeString(string(htmlTagOrNlRe.ReplaceAll(zbn[q[2]:q[3]], repl)))
		}
	}

	log.Debug(nil, "%s: Page: %#v", s.name, p)
	err = osutil.WriteFile(htmlPath, zb, true)
	return
}

func tagAndDir(s string) (tag, dir string) {
	tag = strings.Trim(s, "/")
	if k := strings.Index(tag, "/"); k != -1 {
		dir = tag[k+1:]
		tag = tag[:k]
	}
	return
}

func toTitle(s string) string {
	s = strings.Replace(s, "-", " ", -1)
	s = strings.Title(s)
	return s
}

func parsePageTime(s string) (t time.Time) {
	var err error
	for _, f := range pageTimeFmts {
		t, err = time.Parse(f, s)
		if err == nil {
			return
		}
	}
	return
}

func formatTime(t time.Time) string {
	return t.Format(time.RFC1123)
}

func relativePath(parent, fpath string) (relpath string, err error) {
	relpath, err = filepath.Rel(parent, fpath)
	if err != nil {
		return
	}
	// cannot use if relpath[0] == '.', as it fails for path names starting with dot
	if relpath == "." {
		relpath = ""
	}
	if filepath.Separator != '/' {
		relpath = strings.Replace(relpath, string(filepath.Separator), "/", -1)
	}
	return
}

func writeStaticFile(fpath string, buf []byte, doGzip bool, p *pool.T) (err error) {
	if err = osutil.WriteFile(fpath, buf, true); err != nil || !doGzip {
		return
	}
	fpath2 := fpath + ".gz"
	f, err := os.Create(fpath2)
	if err != nil {
		return
	}
	defer f.Close()
	gw := pool.Must(p.Get(0)).(*gzip.Writer)
	gw.Reset(f)
	io.Copy(gw, bytes.NewBuffer(buf))
	err = gw.Close()
	p.Put(gw)
	return
}
