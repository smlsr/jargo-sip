package sip

// ReadDTMF returns one telephone-event press. Events 12-15 and flash are
// returned here and are not keypad frames. Keypad events are returned here
// as well; the pipeline adapter reads its own copy of events 0-11.
func (c *Call) ReadDTMF() (DTMF, error) {
	if err := c.mediaUp(); err != nil {
		return DTMF{}, err
	}
	return c.dlg.media.ReadDTMF()
}

// WriteDTMF queues a telephone-event after PCM already waiting.
func (c *Call) WriteDTMF(ev DTMF) error {
	if err := c.mediaUp(); err != nil {
		return err
	}
	return c.dlg.media.WriteDTMF(ev)
}

func (c *Call) mediaUp() error {
	if c == nil || c.dlg == nil || c.dlg.media == nil {
		return ErrMediaClosed
	}
	select {
	case <-c.done:
		return ErrMediaClosed
	default:
		return nil
	}
}

// readKeypad returns the next event in 0-11. Events 12-15 and flash stay on
// ReadDTMF and are not delivered here.
func (c *Call) readKeypad() (DTMF, error) {
	if err := c.mediaUp(); err != nil {
		return DTMF{}, err
	}
	return c.dlg.media.readKeypad()
}

func (m *Media) readKeypad() (DTMF, error) {
	if m == nil || m.tones == nil {
		return DTMF{}, ErrMediaClosed
	}
	return m.tones.readKey()
}

func (d *dtmfRecv) readKey() (DTMF, error) {
	if d == nil {
		return DTMF{}, ErrMediaClosed
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.keys) == 0 && !d.closed {
		d.cond.Wait()
	}
	if len(d.keys) == 0 {
		return DTMF{}, ErrMediaClosed
	}
	ev := d.keys[0]
	d.keys[0] = DTMF{}
	d.keys = d.keys[1:]
	return ev, nil
}
