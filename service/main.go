package main

import (
	"flag"
	"fmt"
	stdlog "log"
	"math/rand"
	"net/http"
	"os"
	"strings"

	"github.com/kardianos/service"
	"github.com/nextdns/nextdns/endpoint"

	"github.com/rs/nextdns-windows/ctl"
	"github.com/rs/nextdns-windows/proxy"
	"github.com/rs/nextdns-windows/settings"
	"github.com/rs/nextdns-windows/updater"
)

const upstreamBase = "https://dns.nextdns.io/"

var log service.Logger

type proxySvc struct {
	proxy.Proxy
	router endpoint.Manager
	ctl    ctl.Server
}

func (p *proxySvc) Start(s service.Service) error {
	return p.ctl.Start()
}

func (p *proxySvc) Stop(s service.Service) error {
	err := p.Proxy.Stop()
	if err != nil {
		return err
	}
	return p.ctl.Stop()
}

func main() {
	stdlog.SetOutput(os.Stdout)
	svcFlag := flag.String("service", "", fmt.Sprintf("Control the system service.\nValid actions: %s", strings.Join(service.ControlAction[:], ", ")))
	flag.Parse()

	up := &updater.Updater{
		URL: "https://storage.googleapis.com/nextdns_windows/info.json",
	}
	up.SetAutoRun(!settings.Load().DisableCheckUpdate)

	var p *proxySvc
	p = &proxySvc{
		proxy.Proxy{
			Upstream: upstreamBase + settings.Load().Configuration,
		},
		endpoint.Manager{
			Providers: []endpoint.Provider{
				// Prefer unicast routing.
				endpoint.SourceURLProvider{
					SourceURL: "https://router.nextdns.io",
					Client: &http.Client{
						// Trick to avoid depending on DNS to contact the router API.
						Transport: endpoint.NewTransport(
							endpoint.New("router.nextdns.io", "", []string{
								"216.239.32.21",
								"216.239.34.21",
								"216.239.36.21",
								"216.239.38.21",
							}[rand.Intn(3)])),
					},
				},
				// Fallback on anycast.
				endpoint.StaticProvider(endpoint.New("dns1.nextdns.io", "", "45.90.28.0")),
				endpoint.StaticProvider(endpoint.New("dns2.nextdns.io", "", "45.90.30.0")),
				// Fallback on CDN fronting.
				endpoint.StaticProvider(endpoint.New("d1xovudkxbl47e.cloudfront.net", "", "")),
			},
			OnError: func(e endpoint.Endpoint, err error) {
				_ = log.Warningf("Endpoint failed: %s: %v", e.Hostname, err)
			},
			OnChange: func(e endpoint.Endpoint, rt http.RoundTripper) {
				_ = log.Infof("Switching endpoint: %s", e.Hostname)
				p.Transport = rt
			},
		},
		ctl.Server{
			Namespace: "NextDNS",
			OnStart: func() {
				s := settings.Load()
				if s.Enabled {
					_ = p.Proxy.Start()
				}
			},
			Handler: ctl.EventHandlerFunc(func(e ctl.Event) {
				_ = log.Infof("received event: %s %v", e.Name, e.Data)
				switch e.Name {
				case "open":
					// Use to open the GUI window in the existing instance of
					// the app when a duplicate instance is open.
					_ = p.ctl.Broadcast(ctl.Event{Name: "open"})
				case "enable", "disable", "status":
					var err error
					switch e.Name {
					case "enable":
						err = p.Proxy.Start()
					case "disable":
						err = p.Proxy.Stop()
					}
					if e.Name != "status" {
						s := settings.Load()
						s.Enabled = e.Name == "enable"
						if err := s.Save(); err != nil {
							p.ErrorLog(fmt.Errorf("cannot write settings: %v", err))
						}
					}
					if err != nil {
						_ = p.ctl.Broadcast(ctl.Event{
							Name: "error",
							Data: map[string]interface{}{
								"error": err.Error(),
							},
						})
					}
					status, _ := p.Proxy.Started()
					_ = p.ctl.Broadcast(ctl.Event{
						Name: "status",
						Data: map[string]interface{}{
							"enabled": status,
						},
					})
				case "settings":
					if e.Data != nil {
						s := settings.FromMap(e.Data)
						if err := s.Save(); err != nil {
							p.ErrorLog(fmt.Errorf("cannot write settings: %v", err))
						}
						p.Upstream = upstreamBase + s.Configuration
						up.SetAutoRun(!s.DisableCheckUpdate)
					}
					_ = p.ctl.Broadcast(ctl.Event{
						Name: "settings",
						Data: settings.Load().ToMap(),
					})
				default:
					p.ErrorLog(fmt.Errorf("invalid event: %v", e))
				}
			}),
		},
	}

	svcConfig := &service.Config{
		Name:        "NextDNSService",
		DisplayName: "NextDNS Service",
		Description: "NextDNS DNS53 to DoH proxy.",
	}
	s, err := service.New(p, svcConfig)
	if err != nil {
		stdlog.Fatal(err)
	}
	errs := make(chan error, 5)
	if log, err = s.Logger(errs); err != nil {
		stdlog.Fatal(err)
	}
	go func() {
		for {
			err := <-errs
			if err != nil {
				stdlog.Print(err)
			}
		}
	}()
	p.QueryLog = func(qname string) {
		_ = log.Infof("resolve %s", qname)
	}
	p.InfoLog = func(msg string) {
		_ = log.Info(msg)
	}
	p.ErrorLog = func(err error) {
		_ = log.Error(err)
	}
	p.ctl.ErrorLog = func(err error) {
		_ = log.Error(err)
	}
	up.OnUpgrade = func(newVersion string) {
		_ = log.Infof("upgrading from %s to %s", updater.CurrentVersion(), newVersion)
	}
	up.ErrorLog = func(err error) {
		_ = log.Error(err)
	}
	if len(*svcFlag) != 0 {
		err := service.Control(s, *svcFlag)
		if err != nil {
			stdlog.Fatal(err)
		}
		return
	}
	if err = s.Run(); err != nil {
		_ = log.Error(err)
	}
}
