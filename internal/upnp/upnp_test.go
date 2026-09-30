package upnp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const desc = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
 <device>
  <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
  <deviceList><device>
   <deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
   <deviceList><device>
    <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
    <serviceList><service>
     <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
     <controlURL>/ctl/IPConn</controlURL>
    </service></serviceList>
   </device></deviceList>
  </device></deviceList>
 </device>
</root>`

type fakeIGD struct {
	mu       sync.Mutex
	mappings map[string]string
	calls    []string
}

func (f *fakeIGD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/desc.xml" {
		_, _ = io.WriteString(w, desc)
		return
	}
	body, _ := io.ReadAll(r.Body)
	action := r.Header.Get("SOAPAction")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, action)
	fail := func(code, text string) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>`+code+`</errorCode><errorDescription>`+text+`</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
	}
	switch {
	case strings.HasSuffix(action, `#GetExternalIPAddress"`):
		_, _ = io.WriteString(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:GetExternalIPAddressResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"><NewExternalIPAddress>203.0.113.50</NewExternalIPAddress></u:GetExternalIPAddressResponse></s:Body></s:Envelope>`)
	case strings.HasSuffix(action, `#AddPortMapping"`):
		port := between(string(body), "<NewExternalPort>", "</NewExternalPort>")
		lease := between(string(body), "<NewLeaseDuration>", "</NewLeaseDuration>")
		switch {
		case port == "42525":
			fail("718", "ConflictInMappingEntry")
		case lease != "0":
			fail("725", "OnlyPermanentLeasesSupported")
		default:
			f.mappings[port] = between(string(body), "<NewInternalClient>", "</NewInternalClient>")
			_, _ = io.WriteString(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body/></s:Envelope>`)
		}
	case strings.HasSuffix(action, `#DeletePortMapping"`):
		delete(f.mappings, between(string(body), "<NewExternalPort>", "</NewExternalPort>"))
		_, _ = io.WriteString(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body/></s:Envelope>`)
	default:
		fail("401", "Invalid Action")
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		return s[:j]
	}
	return ""
}

func TestMapFlow(t *testing.T) {
	f := &fakeIGD{mappings: map[string]string{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	c, err := FromLocation(ctx, srv.URL+"/desc.xml")
	if err != nil {
		t.Fatal(err)
	}
	if c.ControlURL != srv.URL+"/ctl/IPConn" || !strings.Contains(c.ServiceType, "WANIPConnection") {
		t.Fatalf("client %+v", c)
	}
	ip, err := c.ExternalIP(ctx)
	if err != nil || ip.String() != "203.0.113.50" {
		t.Fatalf("external ip %v %v", ip, err)
	}
	ext, lease, err := c.Map(ctx, 42525, "test & <stuff>")
	if err != nil {
		t.Fatal(err)
	}
	if ext == 42525 || lease != 0 {
		t.Fatalf("expected a different port with a permanent lease, got %d lease %d", ext, lease)
	}
	if len(f.mappings) != 1 {
		t.Fatalf("mappings %v", f.mappings)
	}
	if err := c.Unmap(ctx, ext); err != nil {
		t.Fatal(err)
	}
	if len(f.mappings) != 0 {
		t.Fatalf("mapping not removed: %v", f.mappings)
	}
}
