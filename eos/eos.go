// eos stands for 'enhanced os', it mostly supplies 'eos.Open', which supports
// the 'itchfs://' scheme to access remote files
package eos

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/itchio/wharf/eos/httpfile"
	"github.com/itchio/wharf/eos/option"
)

type File interface {
	io.Reader
	io.Closer
	io.ReaderAt

	Stat() (os.FileInfo, error)
}

type Handler interface {
	Scheme() string
	MakeResource(u *url.URL) (httpfile.Resource, error)
}

var handlers = make(map[string]Handler)

func RegisterHandler(h Handler) error {
	scheme := h.Scheme()

	if handlers[scheme] != nil {
		return fmt.Errorf("already have a handler for %s:", scheme)
	}

	handlers[h.Scheme()] = h
	return nil
}

func DeregisterHandler(h Handler) {
	delete(handlers, h.Scheme())
}

type simpleHTTPResource struct {
	url string
}

func (shr *simpleHTTPResource) GetURL() (string, error) {
	return shr.url, nil
}

func (shr *simpleHTTPResource) NeedsRenewal(req *http.Request) bool {
	return false
}

func Open(name string, opts ...option.Option) (File, error) {
	settings := option.DefaultSettings()

	for _, opt := range opts {
		opt.Apply(settings)
	}

	u, err := url.Parse(name)
	if err != nil {
		return nil, err
	}

	switch u.Scheme {
	case "":
		return os.Open(name)
	case "http", "https":
		res := &simpleHTTPResource{name}
		return httpfile.New(res, settings.HTTPClient)
	default:
		handler := handlers[u.Scheme]
		if handler == nil {
			return nil, fmt.Errorf("unsupported scheme: %s", u.Scheme)
		}

		res, err := handler.MakeResource(u)
		if err != nil {
			return nil, err
		}

		return httpfile.New(res, settings.HTTPClient)
	}
}