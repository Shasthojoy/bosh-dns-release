package server

import (
	"bosh-dns/dns/server/debughttp"
	"errors"
	"sync"
	"time"

	"github.com/cloudfoundry/bosh-utils/logger"
)

//go:generate counterfeiter . DNSServer

type DNSServer interface {
	ListenAndServe() error
	Shutdown() error
}

type Server struct {
	servers         []DNSServer
	upchecks        []Upcheck
	debugHTTPServer debughttp.Server
	timeout         time.Duration
	upcheckInterval time.Duration
	shutdownChan    chan struct{}
	logger          logger.Logger
}

func New(servers []DNSServer, upchecks []Upcheck, debugHTTPServer debughttp.Server, timeout, upcheckInterval time.Duration, shutdownChan chan struct{}, logger logger.Logger) Server {
	return Server{
		servers:         servers,
		upchecks:        upchecks,
		debugHTTPServer: debugHTTPServer,
		timeout:         timeout,
		shutdownChan:    shutdownChan,
		upcheckInterval: upcheckInterval,
		logger:          logger,
	}
}

func (s Server) Run() error {
	err := make(chan error)
	s.listenAndServe(err)

	done := make(chan struct{})
	s.doUpchecks(done)

	select {
	case e := <-err:
		return e
	case <-time.After(s.timeout):
		return errors.New("timed out waiting for server to bind")
	case <-done:
		s.logger.Debug("server", "done with upchecks")
	}

	s.monitorUpchecks()

	select {
	case <-s.shutdownChan:
		return s.shutdown()
	}
}

func (s Server) monitorUpchecks() {
	for _, upcheck := range s.upchecks {
		go func(h Upcheck, limit int) {
			danger := 0
			for {
				if err := h.IsUp(); err != nil {
					danger += 1
					if danger >= limit && s.shutdownChan != nil {
						close(s.shutdownChan)
						s.shutdownChan = nil
						return
					}
				} else {
					danger = 0
				}

				time.Sleep(s.upcheckInterval)
			}
		}(upcheck, 5)
	}
}

func (s Server) doUpchecks(done chan struct{}) {
	wg := &sync.WaitGroup{}
	wg.Add(len(s.upchecks))

	if len(s.upchecks) == 0 {
		s.logger.Warn("server", "proceeding immediately: no upchecks configured")
		close(done)
		return
	}

	go func() {
		wg.Wait()
		close(done)
	}()

	for _, upcheck := range s.upchecks {
		go func(upcheck Upcheck) {
			for {
				var err error
				if err = upcheck.IsUp(); err == nil {
					break
				}
				s.logger.Debug("upcheck", "waiting for server to come up", err)

				time.Sleep(50 * time.Millisecond)
			}

			wg.Done()
		}(upcheck)
	}
}

func (s Server) listenAndServe(err chan error) {
	for _, server := range s.servers {
		go func(server DNSServer) {
			err <- server.ListenAndServe()
		}(server)
	}

	if s.debugHTTPServer != nil {
		s.debugHTTPServer.Serve()
	}
}

func (s Server) shutdown() error {
	err := make(chan error, len(s.servers))

	wg := &sync.WaitGroup{}
	wg.Add(len(s.servers))

	for _, server := range s.servers {
		go func(server DNSServer) {
			err <- server.Shutdown()

			wg.Done()
		}(server)
	}

	wg.Wait()
	close(err)

	for e := range err {
		if e != nil {
			return e
		}
	}

	return nil
}
