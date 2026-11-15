package sip

import (
	"log"
	"net"
	"time"

	"github.com/pion/rtcp"
)

// reportGap stops a received sender report from answering itself in a loop.
const reportGap = 50 * time.Millisecond

func (m *Media) readRTCP() {
	defer m.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, addr, err := m.rtcp.ReadFromUDP(buf)
		if err != nil {
			if m.readDone(err, m.rtcp) {
				return
			}
			continue
		}
		m.takeRTCP(buf[:n], addr, time.Now())
	}
}

func (m *Media) rtcpLoop() {
	defer m.wg.Done()
	interval := m.interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			m.writeReport(true, false)
			return
		case <-ticker.C:
			m.writeReport(false, false)
		case <-m.kick:
			m.writeReport(false, true)
		}
	}
}

func (m *Media) takeRTCP(b []byte, addr *net.UDPAddr, now time.Time) {
	if !compoundOK(b) {
		if looksLikeRTP(b) {
			m.markRTPOnRTCP()
		}
		return
	}
	pkts, err := rtcp.Unmarshal(b)
	if err != nil || len(pkts) == 0 || !firstRTCP(pkts[0]) {
		return
	}
	m.rtcpLatch.observe(addr, now)
	sawSR := false
	for _, pkt := range pkts {
		switch p := pkt.(type) {
		case *rtcp.SenderReport:
			sawSR = true
			m.recv.noteSR(p.SSRC, p.NTPTime, now)
			for _, rep := range p.Reports {
				m.notePeerReport(rep, now)
			}
		case *rtcp.ReceiverReport:
			for _, rep := range p.Reports {
				m.notePeerReport(rep, now)
			}
		case *rtcp.Goodbye:
			m.noteBye(addr, p)
		}
	}
	if sawSR {
		m.mu.Lock()
		m.srIn++
		m.mu.Unlock()
		m.watchRequest()
	}
}

func (m *Media) writeReport(bye, kicked bool) {
	if kicked && !bye && m.reportedRecently() {
		return
	}
	stats := m.send.stats()
	now := time.Now()
	var pkts []rtcp.Packet
	if stats.Packets > 0 {
		ntp := ntp64(now)
		sr := &rtcp.SenderReport{
			SSRC:        stats.SSRC,
			NTPTime:     ntp,
			RTPTime:     stats.LastTimestamp,
			PacketCount: stats.Packets,
			OctetCount:  stats.Octets,
		}
		if rep, ok := m.reportBlock(now); ok {
			sr.Reports = []rtcp.ReceptionReport{rep}
		}
		pkts = append(pkts, sr)
		m.rememberSR(uint32(ntp>>16), now)
	} else if rep, ok := m.reportBlock(now); ok {
		pkts = append(pkts, &rtcp.ReceiverReport{
			SSRC:    stats.SSRC,
			Reports: []rtcp.ReceptionReport{rep},
		})
	} else if !bye {
		return
	}
	if bye {
		pkts = append(pkts, &rtcp.Goodbye{Sources: []uint32{stats.SSRC}})
	}
	pkts = append(pkts, rtcp.NewCNAMESourceDescription(stats.SSRC, m.cname))
	raw, err := rtcp.Marshal(pkts)
	if err != nil {
		log.Printf("sip: RTCP marshal: %v", err)
		return
	}
	dest := m.rtcpLatch.dest()
	if dest == nil {
		return
	}
	if _, err := m.rtcp.WriteToUDP(raw, dest); err != nil {
		return
	}
	m.mu.Lock()
	m.lastReport = now
	m.mu.Unlock()
}

// reportBlock is one receiver report. Fraction lost and the cumulative
// count come from the jitter buffer. The interarrival jitter, LSR, and
// DLSR stay on the reception clock. RTT is not applied to the playout target.
func (m *Media) reportBlock(now time.Time) (rtcp.ReceptionReport, bool) {
	rep, ok := m.recv.report(now)
	if !ok {
		return rtcp.ReceptionReport{}, false
	}
	loss, have := m.play.loss()
	if !have {
		return rep, true
	}
	rep.SSRC = loss.ssrc
	rep.FractionLost = loss.frac
	rep.TotalLost = loss.total
	rep.LastSequenceNumber = loss.ext
	return rep, true
}

func (m *Media) reportedRecently() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.lastReport.IsZero() && time.Since(m.lastReport) < reportGap
}

func (m *Media) rememberSR(lsr uint32, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.srMarks = append(m.srMarks, srMark{lsr: lsr, at: at})
	if len(m.srMarks) > 8 {
		m.srMarks = append([]srMark(nil), m.srMarks[len(m.srMarks)-8:]...)
	}
}

func (m *Media) notePeerReport(rep rtcp.ReceptionReport, now time.Time) {
	if rep.LastSenderReport == 0 {
		return
	}
	stats := m.send.stats()
	if rep.SSRC != stats.SSRC {
		return
	}
	m.mu.Lock()
	var sent time.Time
	found := false
	for _, mark := range m.srMarks {
		if mark.lsr == rep.LastSenderReport {
			sent = mark.at
			found = true
		}
	}
	m.mu.Unlock()
	if !found {
		return
	}
	dlsr := time.Duration(rep.Delay) * time.Second / 65536
	rtt := now.Sub(sent) - dlsr
	if rtt < 0 {
		return
	}
	log.Printf("sip: RTCP RTT %s", rtt)
	m.mu.Lock()
	m.rtt = rtt
	m.haveRTT = true
	m.mu.Unlock()
}

func (m *Media) noteBye(addr *net.UDPAddr, bye *rtcp.Goodbye) {
	var ssrc uint32
	if len(bye.Sources) > 0 {
		ssrc = bye.Sources[0]
	}
	log.Printf("sip: RTCP BYE from %s ssrc %d", addr, ssrc)
	m.mu.Lock()
	m.byeCount++
	m.mu.Unlock()
}

func (m *Media) markRTPOnRTCP() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rtpOnRTCP {
		return
	}
	m.rtpOnRTCP = true
	log.Printf("sip: RTP on the RTCP socket")
}

func compoundOK(b []byte) bool {
	if len(b) < 4 || b[0]>>6 != 2 {
		return false
	}
	pt := b[1]
	return pt >= 200 && pt <= 204
}

func firstRTCP(pkt rtcp.Packet) bool {
	switch pkt.(type) {
	case *rtcp.SenderReport, *rtcp.ReceiverReport, *rtcp.SourceDescription, *rtcp.Goodbye, *rtcp.ApplicationDefined:
		return true
	default:
		return false
	}
}

func looksLikeRTP(b []byte) bool {
	if len(b) < 12 || b[0]>>6 != 2 {
		return false
	}
	return b[1]&0x7F < 200
}

func ntp64(t time.Time) uint64 {
	const unixToNTP = 2208988800
	sec := uint64(t.Unix()) + unixToNTP
	frac := (uint64(t.Nanosecond()) << 32) / 1e9
	return sec<<32 | frac
}
