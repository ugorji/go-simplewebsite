package simplewebsite

import (
	// "encoding/base64"
	"strings"
	"time"
)

// Page can be read from a json file, so we should also keep some top-level fields

// Summary is the first paragraph,
// or first 2 paragraphs if the second paragraph directly follows the first.
type Summary [2]string

func (s Summary) ToString(sep string) string {
	if s[1] == "" {
		return s[0]
	}
	return s[0] + sep + s[1]
}

func (s Summary) String() string { return s.ToString(" ") }

type Page struct {
	Username string
	Tags     []string
	modTimes []time.Time // time.RFC3339: 2006-01-02T15:04:05Z07:00
	name     string
	title    string
	summary  Summary
	server   *Server
	toc      bool
}

func (p *Page) Author() (u *User) {
	n := p.Username
	if n == "" {
		n = p.server.DefaultUser
	}
	if n != "" {
		u = p.server.User(n)
	}
	return
}

func (p *Page) Name() string {
	return p.name
}

func (p *Page) Server() *Server {
	return p.server
}

func (p *Page) CreateTime() time.Time {
	if len(p.modTimes) == 0 {
		return time.Now().UTC()
	}
	return p.modTimes[0]
}

func (p *Page) LastModTime() time.Time {
	if len(p.modTimes) == 0 {
		return time.Now().UTC()
	}
	return p.modTimes[len(p.modTimes)-1]
}

func (p *Page) ReadableTimestamp() string {
	const timefmt = "02 Jan 2006" // time.RFC1123 = "Mon, 02 Jan 2006 15:04:05 MST"
	switch len(p.modTimes) {
	case 0:
		return time.Now().UTC().Format(timefmt)
	case 1:
		return p.modTimes[0].Format(timefmt)
	default:
		ix := len(p.modTimes) - 1
		if p.modTimes[0].Equal(p.modTimes[ix]) {
			return p.modTimes[0].Format(timefmt)
		} else {
			return p.modTimes[0].Format(timefmt) +
				" (updated " + p.modTimes[ix].Format(timefmt) + ") "
		}
	}
}

func (p *Page) SplitDirAndBase() (dir string, base string) {
	i := strings.LastIndex(p.name, "/")
	if i == -1 {
		return "", p.name
	}
	return p.name[:i], p.name[i+1:]
}

func (p *Page) Dir() (dir string) {
	dir, _ = p.SplitDirAndBase()
	return
}

func (p *Page) Title() string {
	if p.title != "" {
		return p.title
	}
	_, base := p.SplitDirAndBase()
	return toTitle(base)
}

func (p *Page) Summary() string {
	return p.summary.String()
}

func (p *Page) SummaryWithSep(sep string) string {
	return p.summary.ToString(sep)
}

type sortedPagesRecent []*Page

func (x sortedPagesRecent) Len() int           { return len(x) }
func (x sortedPagesRecent) Less(i, j int) bool { return x[i].LastModTime().After(x[j].LastModTime()) }
func (x sortedPagesRecent) Swap(i, j int)      { x[i], x[j] = x[j], x[i] }
