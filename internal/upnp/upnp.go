// Package upnp asks a home router (a UPnP Internet Gateway Device) to forward
// a TCP port to this computer, so friends can connect without a relay.
package upnp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one router's port-mapping service.
type Client struct {
	ControlURL  string
	ServiceType string
	LocalIP     string
	http        *http.Client
}

// SOAPError is a refusal from the router.
type SOAPError struct {
	Code        int
	Description string
	Status      int
}

func (e *SOAPError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("router said no (UPnP error %d %s)", e.Code, e.Description)
	}
	return fmt.Sprintf("router answered HTTP %d", e.Status)
}

// ErrNoGateway means no router answered the discovery request.
var ErrNoGateway = errors.New("no UPnP router answered")

var searchTargets = []string{
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
	"urn:schemas-upnp-org:service:WANIPConnection:1",
	"urn:schemas-upnp-org:service:WANIPConnection:2",
	"urn:schemas-upnp-org:service:WANPPPConnection:1",
}

// Discover finds a router on the local network with SSDP.
func Discover(ctx context.Context, wait time.Duration) (*Client, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	dst := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	for round := 0; round < 2; round++ {
		for _, st := range searchTargets {
			msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nST: " + st + "\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\n\r\n"
			_, _ = conn.WriteToUDP([]byte(msg), dst)
		}
	}
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	seen := map[string]bool{}
	lastErr := ErrNoGateway
	buf := make([]byte, 4096)
	for {
		_ = conn.SetReadDeadline(deadline)
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return nil, lastErr
		}
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
		if err != nil {
			continue
		}
		loc := resp.Header.Get("Location")
		if loc == "" || seen[loc] {
			continue
		}
		seen[loc] = true
		c, err := FromLocation(ctx, loc)
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
}

type device struct {
	Services []service `xml:"serviceList>service"`
	Devices  []device  `xml:"deviceList>device"`
}

type service struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
}

type rootDesc struct {
	URLBase string `xml:"URLBase"`
	Device  device `xml:"device"`
}

// FromLocation reads a router's device description and finds its port-mapping service.
func FromLocation(ctx context.Context, location string) (*Client, error) {
	hc := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var root rootDesc
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&root); err != nil {
		return nil, fmt.Errorf("reading router description: %w", err)
	}
	svc := findService(root.Device)
	if svc == nil {
		return nil, errors.New("router doesn't offer port forwarding over UPnP")
	}
	base, err := url.Parse(location)
	if err != nil {
		return nil, err
	}
	if b, err := url.Parse(strings.TrimSpace(root.URLBase)); err == nil && b.Host != "" {
		base = b
	}
	ref, err := url.Parse(strings.TrimSpace(svc.ControlURL))
	if err != nil {
		return nil, err
	}
	control := base.ResolveReference(ref)
	return &Client{ControlURL: control.String(), ServiceType: strings.TrimSpace(svc.ServiceType), LocalIP: localIPFor(control.Host), http: hc}, nil
}

func findService(d device) *service {
	var ppp *service
	var walk func(d device) *service
	walk = func(d device) *service {
		for i := range d.Services {
			st := d.Services[i].ServiceType
			if strings.Contains(st, ":WANIPConnection:") {
				return &d.Services[i]
			}
			if ppp == nil && strings.Contains(st, ":WANPPPConnection:") {
				ppp = &d.Services[i]
			}
		}
		for _, sub := range d.Devices {
			if s := walk(sub); s != nil {
				return s
			}
		}
		return nil
	}
	if s := walk(d); s != nil {
		return s
	}
	return ppp
}

func localIPFor(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	c, err := net.Dial("udp4", net.JoinHostPort(host, "1900"))
	if err != nil {
		return ""
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

func (c *Client) soap(ctx context.Context, action string, args [][2]string) ([]byte, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?>` + "\r\n" +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&b, `<u:%s xmlns:u="%s">`, action, c.ServiceType)
	for _, a := range args {
		var esc bytes.Buffer
		_ = xml.EscapeText(&esc, []byte(a[1]))
		fmt.Fprintf(&b, "<%s>%s</%s>", a[0], esc.String(), a[0])
	}
	fmt.Fprintf(&b, `</u:%s></s:Body></s:Envelope>`, action)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ControlURL, strings.NewReader(b.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	// Set directly so the header keeps this exact spelling; some routers care.
	req.Header["SOAPAction"] = []string{fmt.Sprintf(`"%s#%s"`, c.ServiceType, action)}
	hc := c.http
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		se := &SOAPError{Status: resp.StatusCode, Description: strings.TrimSpace(xmlValue(data, "errorDescription"))}
		se.Code, _ = strconv.Atoi(strings.TrimSpace(xmlValue(data, "errorCode")))
		return nil, se
	}
	return data, nil
}

func xmlValue(data []byte, name string) string {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == name {
			var v string
			if dec.DecodeElement(&v, &se) != nil {
				return ""
			}
			return v
		}
	}
}

// ExternalIP asks the router for its internet-facing address.
func (c *Client) ExternalIP(ctx context.Context) (netip.Addr, error) {
	data, err := c.soap(ctx, "GetExternalIPAddress", nil)
	if err != nil {
		return netip.Addr{}, err
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(xmlValue(data, "NewExternalIPAddress")))
	if err != nil {
		return netip.Addr{}, errors.New("router didn't report its internet address")
	}
	return ip.Unmap(), nil
}

func (c *Client) add(ctx context.Context, ext, internal, lease int, desc string) error {
	_, err := c.soap(ctx, "AddPortMapping", [][2]string{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(ext)},
		{"NewProtocol", "TCP"},
		{"NewInternalPort", strconv.Itoa(internal)},
		{"NewInternalClient", c.LocalIP},
		{"NewEnabled", "1"},
		{"NewPortMappingDescription", desc},
		{"NewLeaseDuration", strconv.Itoa(lease)},
	})
	return err
}

// Map forwards an external TCP port to internalPort on this computer. It
// returns the external port and the lease in seconds (0 = until removed).
func (c *Client) Map(ctx context.Context, internalPort int, desc string) (ext, lease int, err error) {
	if c.LocalIP == "" {
		return 0, 0, errors.New("couldn't tell which local address the router sees")
	}
	ports := []int{internalPort}
	for i := 0; i < 3; i++ {
		ports = append(ports, 20000+rand.IntN(40000))
	}
	for _, ext = range ports {
		lease = 3600
		err = c.add(ctx, ext, internalPort, lease, desc)
		var se *SOAPError
		if err != nil && errors.As(err, &se) && se.Code != 718 {
			// Many routers only accept permanent mappings (error 725, or a vaguer one).
			lease = 0
			err = c.add(ctx, ext, internalPort, lease, desc)
		}
		if err == nil {
			return ext, lease, nil
		}
		if errors.As(err, &se) && se.Code == 718 { // port already mapped to someone else
			continue
		}
		return 0, 0, err
	}
	return 0, 0, err
}

// Renew refreshes an existing mapping.
func (c *Client) Renew(ctx context.Context, ext, internalPort, lease int, desc string) error {
	return c.add(ctx, ext, internalPort, lease, desc)
}

// Unmap removes a mapping.
func (c *Client) Unmap(ctx context.Context, ext int) error {
	_, err := c.soap(ctx, "DeletePortMapping", [][2]string{{"NewRemoteHost", ""}, {"NewExternalPort", strconv.Itoa(ext)}, {"NewProtocol", "TCP"}})
	return err
}
