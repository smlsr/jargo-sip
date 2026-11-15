package sip

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gojargo/jargo/audio/g711"
	"github.com/pion/rtp"
)

const (
	sippSSRC  = 0x51000005
	sippFill  = 0x12
	sippPCMU  = 0
	sippEvent = 101
	sippDigit = 5
	sippGap   = 20 * time.Millisecond
	sippTail  = 800 * time.Millisecond
)

func TestSIPpUACThreeEndPackets(t *testing.T) {
	bin := sippBinary(t)
	ep := startEP(t, sippConfig(36000))
	pkts := endBitMedia()
	call, run := sippCall(t, bin, ep, pkts)
	started := time.Now()
	heard := countPCMU(call)
	ev := oneDigit(t, call, 12*time.Second)
	if ev.Event != sippDigit {
		t.Fatalf("digit %d", ev.Event)
	}
	if time.Since(started) < 4500*time.Millisecond {
		t.Fatalf("digit arrived in %s, before 5s of PCMU", time.Since(started))
	}
	if n := heard.Load(); n < 200 {
		t.Fatalf("PCMU frames = %d, want at least 200 of the 250", n)
	}
	waitDone(t, call, 3*time.Second)
	run.finish(t, 5*time.Second)
}

func TestSIPpUACNoEndBit(t *testing.T) {
	bin := sippBinary(t)
	ep := startEP(t, sippConfig(36400))
	call, run := sippCall(t, bin, ep, openDigitMedia())
	ev := oneDigit(t, call, 5*time.Second)
	if ev.Event != sippDigit {
		t.Fatalf("digit %d", ev.Event)
	}
	waitDone(t, call, 3*time.Second)
	run.finish(t, 5*time.Second)
}

func TestSIPpUASConnectionAddress(t *testing.T) {
	bin := sippBinary(t)
	t.Run("behind-sbc", func(t *testing.T) {
		ep := startEP(t, sippConfig(37000))
		addrs := mustAddrs(t, ep)
		got := uasConnection(t, bin, ep)
		if !got.Equal(addrs.LocalIP) {
			t.Fatalf("c=%v local=%v", got, addrs.LocalIP)
		}
	})
	t.Run("standalone", func(t *testing.T) {
		cfg := sippConfig(37400)
		cfg.NATMode = NATStandalone
		cfg.ListenIP = ""
		// 127.0.0.2 is loopback, so SIPp's reply to the advertised Via still
		// reaches the wildcard socket, and it is not the local route address.
		cfg.AdvertiseIP = "127.0.0.2"
		ep := startEP(t, cfg)
		addrs := mustAddrs(t, ep)
		if addrs.LocalIP.Equal(addrs.AdvertiseIP) {
			t.Fatalf("local %v and advertise %v are the same", addrs.LocalIP, addrs.AdvertiseIP)
		}
		got := uasConnection(t, bin, ep)
		if !got.Equal(addrs.AdvertiseIP) {
			t.Fatalf("c=%v advertise=%v local=%v", got, addrs.AdvertiseIP, addrs.LocalIP)
		}
	})
}

func sippBinary(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("sipp")
	if err != nil {
		t.Skip("sipp binary is absent")
	}
	return p
}

func sippConfig(rtpMin int) Config {
	cfg := baseConfig()
	cfg.RTPPortMin = rtpMin
	cfg.RTPPortMax = rtpMin + 200
	return cfg
}

// endBitMedia is 5s of PCMU, then telephone-event 5, then three end packets.
func endBitMedia() []pcapRTP {
	var pkts []pcapRTP
	seq := uint16(1)
	var ts uint32
	pkts, seq, ts = appendAudio(pkts, seq, ts, 250)
	eventTS := ts
	for i := 0; i < 8; i++ {
		pkts = append(pkts, eventPacket(seq, eventTS, false, uint16((i+1)*frameSamples)))
		seq++
	}
	dur := uint16(8 * frameSamples)
	for i := 0; i < 3; i++ {
		pkts = append(pkts, eventPacket(seq, eventTS, true, dur))
		seq++
	}
	return pkts
}

// openDigitMedia never sets the end bit. The following PCMU timestamp is later,
// which is what completes the press.
func openDigitMedia() []pcapRTP {
	var pkts []pcapRTP
	seq := uint16(1)
	var ts uint32
	pkts, seq, ts = appendAudio(pkts, seq, ts, 5)
	eventTS := ts
	for i := 0; i < 3; i++ {
		pkts = append(pkts, eventPacket(seq, eventTS, false, uint16((i+1)*frameSamples)))
		seq++
	}
	pkts = append(pkts, audioPacket(seq, eventTS+frameSamples))
	return pkts
}

func appendAudio(pkts []pcapRTP, seq uint16, ts uint32, n int) ([]pcapRTP, uint16, uint32) {
	for i := 0; i < n; i++ {
		pkts = append(pkts, audioPacket(seq, ts))
		seq++
		ts += frameSamples
	}
	return pkts, seq, ts
}

func audioPacket(seq uint16, ts uint32) pcapRTP {
	return pcapRTP{
		pt:      sippPCMU,
		seq:     seq,
		ts:      ts,
		payload: bytes.Repeat([]byte{sippFill}, frameSamples),
	}
}

func eventPacket(seq uint16, ts uint32, end bool, dur uint16) pcapRTP {
	return pcapRTP{
		pt:      sippEvent,
		seq:     seq,
		ts:      ts,
		payload: telephonePayload(sippDigit, end, dtmfVolume, dur),
	}
}

type pcapRTP struct {
	pt      uint8
	seq     uint16
	ts      uint32
	payload []byte
}

func sippCall(t *testing.T, bin string, ep *Endpoint, pkts []pcapRTP) (*Call, *sippRun) {
	t.Helper()
	dir := t.TempDir()
	pcap := filepath.Join(dir, "audio.pcap")
	if err := writeAudioPCAP(pcap, pkts); err != nil {
		t.Fatal(err)
	}
	pause := len(pkts)*int(sippGap/time.Millisecond) + int(sippTail/time.Millisecond)
	scenario := filepath.Join(dir, "uac.xml")
	if err := writeUACScenario(scenario, pcap, pause); err != nil {
		t.Fatal(err)
	}
	addrs := mustAddrs(t, ep)
	target := net.JoinHostPort(addrs.LocalIP.String(), strconv.Itoa(addrs.LocalSIPPort))
	run := startSIPp(t, bin, dir, sippArgs(dir, scenario, target)...)
	call := acceptWithin(t, ep, run, 5*time.Second)
	return call, run
}

func writeUACScenario(path, pcap string, pauseMs int) error {
	raw, err := os.ReadFile(filepath.Join("testdata", "sipp", "uac.xml"))
	if err != nil {
		return err
	}
	text := strings.ReplaceAll(string(raw), "PCAPFILE", pcap)
	text = strings.ReplaceAll(text, "PAUSEMS", strconv.Itoa(pauseMs))
	return os.WriteFile(path, []byte(text), 0o644)
}

func uasConnection(t *testing.T, bin string, ep *Endpoint) net.IP {
	t.Helper()
	dir := t.TempDir()
	port := freePort(t)
	scenario, err := filepath.Abs(filepath.Join("testdata", "sipp", "uas.xml"))
	if err != nil {
		t.Fatal(err)
	}
	run := startSIPp(t, bin, dir, sippArgs(dir, scenario, "", "-p", strconv.Itoa(port))...)
	waitBound(t, port, run)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	uri := fmt.Sprintf("sip:sipp@127.0.0.1:%d", port)
	call, err := ep.Dial(ctx, uri)
	if err != nil {
		t.Fatalf("dial: %v\n%s", err, run.dump())
	}
	hctx, hcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer hcancel()
	if err := call.Hangup(hctx); err != nil {
		t.Fatalf("hangup: %v\n%s", err, run.dump())
	}
	run.finish(t, 5*time.Second)
	body, err := os.ReadFile(filepath.Join(dir, "messages.log"))
	if err != nil {
		t.Fatal(err)
	}
	return inviteConnection(t, string(body))
}

func sippArgs(dir, scenario, target string, extra ...string) []string {
	args := []string{
		"-sf", scenario,
		"-m", "1",
		"-l", "1",
		"-s", "bot",
		"-i", "127.0.0.1",
		"-mi", "127.0.0.1",
		"-bind_local",
		"-t", "u1",
		"-nostdin",
		"-timeout", "20s",
		"-timeout_error",
		"-recv_timeout", "8000",
		"-min_rtp_port", "41000",
		"-max_rtp_port", "41100",
		"-trace_msg",
		"-message_file", filepath.Join(dir, "messages.log"),
		"-trace_err",
		"-error_file", filepath.Join(dir, "errors.log"),
	}
	args = append(args, extra...)
	if target != "" {
		args = append(args, target)
	}
	return args
}

type sippRun struct {
	done   chan struct{}
	err    error
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	dir    string
}

func startSIPp(t *testing.T, bin, dir string, args ...string) *sippRun {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	run := &sippRun{done: make(chan struct{}), stdout: &stdout, stderr: &stderr, dir: dir}
	go func() {
		run.err = cmd.Wait()
		close(run.done)
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	return run
}

func (r *sippRun) finish(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-r.done:
		if r.err != nil {
			t.Fatalf("sipp: %v\n%s", r.err, r.dump())
		}
	case <-time.After(d):
		t.Fatalf("sipp still running\n%s", r.dump())
	}
}

func (r *sippRun) dump() string {
	var b strings.Builder
	if r.stdout.Len() > 0 {
		fmt.Fprintf(&b, "stdout:\n%s\n", r.stdout.String())
	}
	if r.stderr.Len() > 0 {
		fmt.Fprintf(&b, "stderr:\n%s\n", r.stderr.String())
	}
	if body, err := os.ReadFile(filepath.Join(r.dir, "errors.log")); err == nil && len(body) > 0 {
		fmt.Fprintf(&b, "errors.log:\n%s\n", body)
	}
	if body, err := os.ReadFile(filepath.Join(r.dir, "messages.log")); err == nil && len(body) > 0 {
		fmt.Fprintf(&b, "messages.log:\n%s\n", body)
	}
	return b.String()
}

func acceptWithin(t *testing.T, ep *Endpoint, run *sippRun, d time.Duration) *Call {
	t.Helper()
	ch := make(chan *Call, 1)
	errc := make(chan error, 1)
	go func() {
		c, err := ep.Accept()
		if err != nil {
			errc <- err
			return
		}
		ch <- c
	}()
	select {
	case c := <-ch:
		return c
	case err := <-errc:
		t.Fatalf("accept: %v\n%s", err, run.dump())
	case <-time.After(d):
		t.Fatalf("Accept timed out\n%s", run.dump())
	}
	return nil
}

func countPCMU(call *Call) *atomic.Int64 {
	want := g711.DecodeULaw(bytes.Repeat([]byte{sippFill}, frameSamples))
	var n atomic.Int64
	go func() {
		for {
			pcm, err := call.ReadPCM()
			if err != nil {
				return
			}
			if bytes.Equal(pcm, want) {
				n.Add(1)
			}
		}
	}()
	return &n
}

func oneDigit(t *testing.T, call *Call, wait time.Duration) DTMF {
	t.Helper()
	type res struct {
		ev  DTMF
		err error
	}
	first := make(chan res, 1)
	go func() {
		ev, err := call.ReadDTMF()
		first <- res{ev, err}
	}()
	var ev DTMF
	select {
	case r := <-first:
		if r.err != nil {
			t.Fatal(r.err)
		}
		ev = r.ev
	case <-time.After(wait):
		t.Fatal("ReadDTMF timed out")
	}
	again := make(chan res, 1)
	go func() {
		next, err := call.ReadDTMF()
		again <- res{next, err}
	}()
	select {
	case r := <-again:
		if r.err == nil {
			t.Fatalf("second digit %d", r.ev.Event)
		}
	case <-call.Done():
		t.Fatal("Done closed before the digit could repeat")
	case <-time.After(400 * time.Millisecond):
	}
	return ev
}

func waitBound(t *testing.T, port int, run *sippRun) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err != nil {
			return
		}
		_ = c.Close()
		select {
		case <-run.done:
			t.Fatalf("sipp exited before bind: %v\n%s", run.err, run.dump())
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("sipp did not bind\n%s", run.dump())
}

func inviteConnection(t *testing.T, log string) net.IP {
	t.Helper()
	rest := log
	for {
		i := strings.Index(rest, "message received")
		if i < 0 {
			break
		}
		rest = rest[i:]
		j := strings.Index(rest[len("message received"):], "message sent")
		block := rest
		if j >= 0 {
			block = rest[:len("message received")+j]
			rest = rest[len("message received")+j:]
		} else {
			rest = ""
		}
		if !strings.Contains(block, "INVITE ") {
			continue
		}
		m := regexp.MustCompile(`c=IN IP4 ([0-9.]+)`).FindStringSubmatch(block)
		if m == nil {
			t.Fatalf("INVITE has no c= line:\n%s", block)
		}
		ip := net.ParseIP(m[1])
		if ip == nil {
			t.Fatalf("c=%q", m[1])
		}
		return ip
	}
	t.Fatalf("INVITE was not in the SIPp log:\n%s", log)
	return nil
}

func writeAudioPCAP(path string, pkts []pcapRTP) error {
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, uint32(0xa1b2c3d4)); err != nil {
		return err
	}
	if err := binary.Write(&buf, binary.LittleEndian, uint16(2)); err != nil {
		return err
	}
	if err := binary.Write(&buf, binary.LittleEndian, uint16(4)); err != nil {
		return err
	}
	if err := binary.Write(&buf, binary.LittleEndian, int32(0)); err != nil {
		return err
	}
	if err := binary.Write(&buf, binary.LittleEndian, uint32(0)); err != nil {
		return err
	}
	if err := binary.Write(&buf, binary.LittleEndian, uint32(65535)); err != nil {
		return err
	}
	if err := binary.Write(&buf, binary.LittleEndian, uint32(1)); err != nil {
		return err
	}
	for i, p := range pkts {
		frame, err := ethernetUDP(p)
		if err != nil {
			return err
		}
		us := int64(time.Second/time.Microsecond) + int64(i)*int64(sippGap/time.Microsecond)
		if err := binary.Write(&buf, binary.LittleEndian, uint32(us/1_000_000)); err != nil {
			return err
		}
		if err := binary.Write(&buf, binary.LittleEndian, uint32(us%1_000_000)); err != nil {
			return err
		}
		if err := binary.Write(&buf, binary.LittleEndian, uint32(len(frame))); err != nil {
			return err
		}
		if err := binary.Write(&buf, binary.LittleEndian, uint32(len(frame))); err != nil {
			return err
		}
		if _, err := buf.Write(frame); err != nil {
			return err
		}
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func ethernetUDP(p pcapRTP) ([]byte, error) {
	raw, err := (&rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    p.pt,
			SequenceNumber: p.seq,
			Timestamp:      p.ts,
			SSRC:           sippSSRC,
		},
		Payload: p.payload,
	}).Marshal()
	if err != nil {
		return nil, err
	}
	udpLen := 8 + len(raw)
	ipLen := 20 + udpLen
	pkt := make([]byte, 14+ipLen)
	for i := 0; i < 6; i++ {
		pkt[i] = 0xff
	}
	pkt[6] = 0x02
	binary.BigEndian.PutUint16(pkt[12:], 0x0800)
	pkt[14] = 0x45
	binary.BigEndian.PutUint16(pkt[16:], uint16(ipLen))
	pkt[22] = 64
	pkt[23] = 17
	copy(pkt[26:30], []byte{127, 0, 0, 1})
	copy(pkt[30:34], []byte{127, 0, 0, 1})
	sum := ipChecksum(pkt[14:34])
	binary.BigEndian.PutUint16(pkt[24:], sum)
	binary.BigEndian.PutUint16(pkt[34:], 4000)
	binary.BigEndian.PutUint16(pkt[36:], 5000)
	binary.BigEndian.PutUint16(pkt[38:], uint16(udpLen))
	copy(pkt[42:], raw)
	return pkt, nil
}

func ipChecksum(h []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(h); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(h[i:]))
	}
	for sum > 0xffff {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
