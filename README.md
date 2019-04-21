# go-simplewebsite/simplewebsite

This repository contains the `go-simplewebsite/simplewebsite` library (or command).

To install:

```
go get github.com/ugorji/go-simplewebsite/simplewebsite
```

# Package Documentation


Package simplewebsite provides a fast and flexible static site generator or
dynamic file server.

simplewebsite is a modern, lean, fast website engine for typical websites,
with mostly static files and some dynamic content.

It is based off templates, markdown files and static files.

Each page (webpage, blog, etc) is represented by 2 files:

    - content: *.page.md or *.page.html for markdown, or html respectively
    - metadata: *.page.json (timestamp, author and tags).
      this metadata file is optional, as this information can be stored directly on the *.page.md file.

You can optionally store the metadata as the first thing in the contents
page, in the form:

```
    <!-- $meta 2011-03-23T19:29:00-07:00 2018-03-23T19:29:00-07:00 ... :u:ugorji :toc  android geek -->
         corresponding to:
    <!-- $meta timestamp_in_RFC3339 timestamp_in_RFC3339 ... option_ option_ option_ tag_ tag_ ... ->
```

Metadata here can be quoted to include spaces, or any sequence of characters
without spaces or commas. Example:

```
    <!-- $meta 2011-03-23T19:29:00-07:00 - android geek "playa playon , double" 'player playon, single' playee-dash -->
```

The content files are converted/copied as html and stored in a _to_html
directory.

Each directory containing pages "may" define a different set of templates.
This way, different directories can show semantically different types of
content, e.g:

    - web pages  (mostly static. part of website)
    - blog posts (blogs)

Template files always end with .page.thtml.

The expected templates:

    - _dir   (for paths like /a/b/)
    - _page     (for paths like /a/b/c. Searched recursively.)
    - _tag      (for tag page request. exist at top level)
    - _error    (for errors. exist at top level)

Note that all templates are bound to "absolute" directories.

    - _dir template for /a/b/ is got from /a/b/
    - _page template for /a/b/c is got from /a/b/ or /a/, or / recursively.

All templates can be found recursively up the tree, except _dir. A _dir
template must be defined at the level. Absense of one suggests that nothing
should be shown for that directory request. This way, we can have branches
for /, and /blog/, but not for /blog/a/b/; meaning that /blog/a/b/ should
not have a branch page.

```
    We considered using the existence of a _index.page.md to indicate index file,
    and allow _dir template be got recursively. However, this will make the
    _index show up as one of the pages in listings, etc. It also makes it hard
    to differentiate. It's best when we assume that a dir is not a page/file, and
    you define how to show directory information separate from pages.
```

We use filepath.Walk which walks files in sorted order. To support multiple
templates, all templates must be organized in lexical order. This way,
templates which are depended upon by others are loaded first.

Old paths (e.g. /project/*) are handled using a redirect. To support this,
we keep a rewrite map in a json file, and update this for any new path.

At startup time, we walk the directory, read in *.page.* files and snapshot
the server. To recreate the server when files change, just reload or restart
the server.

Expected Paths are (in order checked):

    - startWith /_, /.,     : forbidden
    - match for redirect    : redirect
    - has extension         : sendFile via FileServer (images, etc)
    - startWith dynamicPathPfx : dynamic: e.g. /d/tag, etc
    - end with /            : Branch (or 404)
    - if dir                : Branch
    - if page               : Leaf
    - Else                  : 404
    - default               : Leaf

In terms of the code structure, a OS process runs an engine. That engine
holds a number of server objects, one for each base uri that the engine
serves. When a request comes into the engine, we match the hostname to a
server and dispatch to the appropriate server. Each server is self-contained
and can be removed or added to the engine as needed. Also, each server can
reload itself - though this reload doesn't accomodate config changes. The
engine however can be reloaded to read the config changes.

An engine can serve requests for multiple host names. If the host name
matches, it is dispatched to the appropriate server.

At startup, we will write pid to a file. This will allow easy shutdown of
server. A shell script will auto-restart the server if it dies.

Note that a tag is:

    - all lower case
    - no spaces
    - words separated by dashes

A function called "_title" is added to the template funcmap to convert to
title case.

Primary tags can be defined. These tags are:

    - shown on the home page (under recent)
    - show atom subscribe links to them

## The Server will support

    - max concurrent requests
    - access logging
    - signals: HUP to reload. TERM to graceful shutdown. USR1 to reopen logs.

Caching is supported. To fully support it:

    - Always send max-age header to 1 week (site will be updated weekly)
    - Send Last-Modified header.
    - Support Conditional GET for If-Modified-Since.

The utopia model for determining last-mod-time and whether to send 304 is:

    - keep track of lastMod time for templates, and timestamp of pages
    - use the most recent one as the last-mod-time for cache control

However, for now, we just use last reload time for sending last-mod-time and
checking if to return 304.

Support getting pages matching a tag and a base path:

```
    E.g. show pages under /blog which have tag: technology.
```

To make a page featured on the home page, add the tag: x-featured.

Note that the attributes: name, title and summary are not got from the page
metadata. Instead, the name is got from the name of the contents file, and
the title is the first header in the file, and summary is the first 2
paragraphs.

Support users: A user can author a page. Users can be defined on a Server.
If a user is not found on a server, users defined on the engine are
searched. Each server has a default user, who is assumed to be the author of
any page for which an author is not defined.

Support forms: A form is basically a collection of parameters.

When filled and submitted, an email containing the form is sent to the user.
The from and to are in the parameters x-from and x-to respectively.

To use, create a form with the destination ${dynamicPathPrefix}message (e.g.
/d/message), and put all the parameters you need in there.

Typically, use this via AJAX since we only use the response code to indicate
success (or response code of 500 and body to indicate failure).

Support extensions for pages: This allows for static site generation.

If you configure the extension for a page, you can access the page using
that extension and it will get served. Note that you can still access the
page without the extension (that still works fine).

Support: configurable dynamic prefix (ie make /d/ configurable).

Support generating static site. It basically generates the full site,
including:

    - links for directories, feeds and tags (index.html)
    - pages

## Support: Serving static gzip files

Gzip files will be created at build time alongside regular files.

serveFile will look for NAME.gz, and if it finds it, decipher the content
type from the regular file, and serve the gzip version, setting the
appropriate headers so that the gzip pipeline will skip it.

Support: Users adding their own dynamic functions (for expansion).

Allow the registration of other dynamic paths, without going deep into this
file.

Support: Server Template.

Users should not have to repeat themselves if most parts of their Server
configuration look the same.

Support: watch/notification for live reload of changed files.

The engine will watch all server directories, and micro-reload changed files
in there. It also watches the config file to reload it if change.

This aids a lot during development.

## Support: static conversion of text

If patterns are seen in the file, replace them with replacements stored
locally. This way, some replacements can be done at build time, as opposed
to only doing within javascript.

A sample replacement file looks like:

```
    ----------BOUNDARY-----------
    ABC(\d+)DEF
    $1
    $1
    $1
    ----------BOUNDARY-----------
    UV([a-z]+)WX(\d+)YZ
    $1 - $1

    $2 - $2

    $1 - $2

    ----------BOUNDARY-----------
```

NOTE:

    - Always prefix your templates, replacements, etc with _ or something
      so they are sorted before your actual pages.
      This way, they are always parsed first before pages may need them.

## Exported Package API

```go
func Main(args []string) (err error)
type Dir struct{ ... }
type DynamicPathFn func(s *Server, w http.ResponseWriter, r *http.Request) error
type Engine struct{ ... }
type Page struct{ ... }
type Runner struct{ ... }
type Server struct{ ... }
type TagStat struct{ ... }
type User struct{ ... }
```
