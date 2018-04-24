package edge

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/mholt/caddy"
	"github.com/sirupsen/logrus"

	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/dnsutil"
)

// The global logger for this plugin.
var log *logrus.Logger

// Registers plugin and initializes logger.
func init() {

	// Initialize logger.
	log = logrus.New()
	log.Out = os.Stdout

	// Register plugin with caddy.
	caddy.RegisterPlugin(pluginName, caddy.Plugin{
		ServerType: "dns",
		Action:     setup,
	})
}

// Specifies everything to be run/configured before serving DNS queries.
func setup(c *caddy.Controller) error {

	// Parse the plugin arguments.
	e, err := parseEdge(c)
	if err != nil {
		return plugin.Error(pluginName, err)
	}

	// Make sure the max number of upstream proxies isn't exceeded.
	if e.NumUpstreams() > maxUpstreams {
		return plugin.Error(pluginName, fmt.Errorf("more than %d TOs configured: %d", maxUpstreams, o.NumUpstreams()))
	}

	// Add the plugin handler to the dnsserver.
	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		e.clientset, err = RegisterKubernetesClient()
		e.Next = next
		return e
	})
	if err != nil {
		return err
	}

	// Declare a startup routine.
	c.OnStartup(func() error {
		log.Infof("Starting %s plugin...\n", pluginName)
		return e.OnStartup()
	})

	// Declare a teardown routine.
	c.OnShutdown(func() error {
		log.Infof("Shutting down %s plugin...\n", pluginName)
		return e.OnShutdown()
	})

	return nil
}

// OnStartup starts reading/pushing services and listening for downstream
// table updates.
func (e *Edge) OnStartup() (err error) {
	e.startReadingServices()
	e.startListeningForTableUpdates()
	meta := EdgeSite{
		IP:        e.ip,
		GeoCoords: e.geoCoords,
	}
	for _, p := range e.proxies {
		p.start(e.hcInterval)
		p.startPushingServices(e.svcPushInterval, meta, e.services)
	}
	return nil
}

// OnShutdown stops all async processes.
func (e *Edge) OnShutdown() error {
	e.stopReadingServices()
	e.stopListeningForTableUpdates()
	for _, p := range e.proxies {
		p.close()
	}
	return nil
}

// Close is a synonym for OnShutdown().
func (e *Edge) Close() { e.OnShutdown() }

// Parse the Corefile token.
func parseEdge(c *caddy.Controller) (*Edge, error) {

	// Initialize a new Edge struct.
	e := New()

	// TODO: CLEAN
	protocols := map[int]int{}

	i := 0
	for c.Next() {
		if i > 0 {
			return nil, plugin.ErrOnce
		}
		i++

		// Parse my IP address.
		if !c.Args(&e.ip) {
			return e, c.ArgErr()
		}

		// Parse the edge cluster's longitude value.
		var lon string
		if !c.Args(&lon) {
			return e, c.ArgErr()
		}
		parsedLon, err := strconv.ParseFloat(lon, 64)
		if err != nil {
			return e, err
		}
		e.lon = parsedLon

		// Parse the latitude value.
		var lat string
		if !c.Args(&lat) {
			return e, c.ArgErr()
		}
		parsedLat, err := strconv.ParseFloat(lat, 64)
		if err != nil {
			return e, err
		}
		e.lat = parsedLat

		// Parse the service read interval.
		var svcReadIntervalSecsString string
		if !c.Args(&svcReadIntervalSecsString) {
			return e, c.ArgErr()
		}
		svcReadIntervalSecs, err := strconv.Atoi(svcReadIntervalSecsString)
		if err != nil {
			return e, err
		}
		e.svcReadInterval = time.Duration(svcReadIntervalSecs) * time.Second

		// Parse the service push interval.
		var svcPushIntervalSecsString string
		if !c.Args(&svcPushIntervalSecsString) {
			return e, c.ArgErr()
		}
		svcPushIntervalSecs, err := strconv.Atoi(svcPushIntervalSecsString)
		if err != nil {
			return e, err
		}
		e.svcPushInterval = time.Duration(svcPushIntervalSecs) * time.Second

		if !c.Args(&e.from) {
			return e, c.ArgErr()
		}
		e.from = plugin.Host(e.from).Normalize()

		to := c.RemainingArgs()
		if len(to) == 0 {
			return e, c.ArgErr()
		}

		// A bit fiddly, but first check if we've got protocols and if so add them back in when we create the proxies.
		protocols = make(map[int]int)
		for i := range to {
			protocols[i], to[i] = protocol(to[i])
		}

		// If parseHostPortOrFile expands a file with a lot of nameserver our accounting in protocols doesn't make
		// any sense anymore... For now: lets don't care.
		toHosts, err := dnsutil.ParseHostPortOrFile(to...)
		if err != nil {
			return e, err
		}

		for i, h := range toHosts {
			// Double check the port, if e.g. is 53 and the transport is TLS make it 853.
			// This can be somewhat annoying because you *can't* have TLS on port 53 then.
			switch protocols[i] {
			case TLS:
				h1, p, err := net.SplitHostPort(h)
				if err != nil {
					break
				}

				// This is more of a bug in // dnsutil.ParseHostPortOrFile that defaults to
				// 53 because it doesn't know about the tls:// // and friends (that should be fixed). Hence
				// Fix the port number here, back to what the user intended.
				if p == "53" {
					h = net.JoinHostPort(h1, "853")
				}
			}

			// We can't set tlsConfig here, because we haven't parsed it yet.
			// We set it below at the end of parseBlock, use nil now.
			p := NewProxy(h, nil /* no TLS */)
			e.proxies = append(e.proxies, p)
		}

		for c.NextBlock() {
			if err := parseBlock(c, e); err != nil {
				return e, err
			}
		}
	}

	if e.tlsServerName != "" {
		e.tlsConfig.ServerName = e.tlsServerName
	}
	for i := range e.proxies {
		// Only set this for proxies that need it.
		if protocols[i] == TLS {
			e.proxies[i].SetTLSConfig(e.tlsConfig)
		}
		e.proxies[i].SetExpire(e.expire)
	}
	return e, nil
}

func parseBlock(c *caddy.Controller, e *Edge) error {
	switch c.Val() {
	case "except":
		ignore := c.RemainingArgs()
		if len(ignore) == 0 {
			return c.ArgErr()
		}
		for i := 0; i < len(ignore); i++ {
			ignore[i] = plugin.Host(ignore[i]).Normalize()
		}
		e.ignored = ignore
	case "max_fails":
		if !c.NextArg() {
			return c.ArgErr()
		}
		n, err := strconv.Atoi(c.Val())
		if err != nil {
			return err
		}
		if n < 0 {
			return fmt.Errorf("max_fails can't be negative: %d", n)
		}
		e.maxfails = uint32(n)
	case "health_check":
		if !c.NextArg() {
			return c.ArgErr()
		}
		dur, err := time.ParseDuration(c.Val())
		if err != nil {
			return err
		}
		if dur < 0 {
			return fmt.Errorf("health_check can't be negative: %d", dur)
		}
		e.hcInterval = dur
	case "force_tcp":
		if c.NextArg() {
			return c.ArgErr()
		}
		e.forceTCP = true
	case "tls":
		args := c.RemainingArgs()
		if len(args) > 3 {
			return c.ArgErr()
		}

		tlsConfig, err := pkgtls.NewTLSConfigFromArgs(args...)
		if err != nil {
			return err
		}
		e.tlsConfig = tlsConfig
	case "tls_servername":
		if !c.NextArg() {
			return c.ArgErr()
		}
		e.tlsServerName = c.Val()
	case "expire":
		if !c.NextArg() {
			return c.ArgErr()
		}
		dur, err := time.ParseDuration(c.Val())
		if err != nil {
			return err
		}
		if dur < 0 {
			return fmt.Errorf("expire can't be negative: %s", dur)
		}
		e.expire = dur
	case "policy":
		if !c.NextArg() {
			return c.ArgErr()
		}
		switch x := c.Val(); x {
		case "random":
			e.p = &random{}
		case "round_robin":
			e.p = &roundRobin{}
		default:
			return c.Errf("unknown policy '%s'", x)
		}

	default:
		return c.Errf("unknown property '%s'", c.Val())
	}

	return nil
}

const maxUpstreams = 15 // Maximum number of upstreams.
