package lan

import (
	"net"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		motd string
		port int
		ok   bool
	}{
		{"[MOTD]Max - New World[/MOTD][AD]54321[/AD]", "Max - New World", 54321, true},
		{"[MOTD][/MOTD][AD]25565[/AD]", "", 25565, true},
		{"[MOTD]x[/MOTD][AD]192.168.1.2:4000[/AD]", "x", 4000, true},
		{"[MOTD]x[/MOTD][AD]0[/AD]", "", 0, false},
		{"[MOTD]x[/MOTD][AD]99999[/AD]", "", 0, false},
		{"garbage", "", 0, false},
		{"[MOTD]x[/MOTD]", "", 0, false},
	}
	for _, c := range cases {
		motd, port, ok := Parse([]byte(c.in))
		if ok != c.ok || (ok && (motd != c.motd || port != c.port)) {
			t.Errorf("Parse(%q) = %q, %d, %v", c.in, motd, port, ok)
		}
	}
}

func TestFormatSanitizes(t *testing.T) {
	msg := Format("evil[/MOTD][AD]1[/AD]\n", 4242)
	motd, port, ok := Parse(msg)
	if !ok || port != 4242 || motd != "evil1" {
		t.Fatalf("got %q %d %v from %q", motd, port, ok, msg)
	}
	if motd, _, _ := Parse(Format("", 1)); motd != "Minecraft world" {
		t.Fatalf("empty motd became %q", motd)
	}
}

func TestAnnouncer(t *testing.T) {
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	a, err := StartAnnouncer(31337, false, []*net.UDPAddr{pc.LocalAddr().(*net.UDPAddr)})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// Inactive: nothing is sent.
	_ = pc.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 256)
	if _, _, err := pc.ReadFromUDP(buf); err == nil {
		t.Fatal("inactive announcer sent something")
	}
	a.Set("Hello", true)
	_ = pc.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err := pc.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	if motd, port, ok := Parse(buf[:n]); !ok || motd != "Hello" || port != 31337 {
		t.Fatalf("got %q", buf[:n])
	}
}
