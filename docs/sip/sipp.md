# SIPp

Owner: JAR-16. These are live calls with the `sipp` binary, covering the two SIPp lines in [the index](README.md#shared-acceptance).

`go test` for `transport/sip` runs them when `sipp` is on `PATH`. The tests skip only when that binary is absent.

## UAC

[uac.xml](../../transport/sip/testdata/sipp/uac.xml) is the caller. The endpoint is the UAS (`behind-sbc`, bind `127.0.0.1`). The test fills `PCAPFILE` and `PAUSEMS`. `PAUSEMS` is the pcap length plus 800 ms, so BYE is sent after the audio and the digit has already been read once.

After ACK the scenario waits 300 ms, so RTP is armed before the pcap starts, then plays the file.

The end-bit pcap is 5 s of PCMU: 250 packets, 20 ms apart, payload type 0, one SSRC. Then telephone-event 5 at payload 101. Eight packets leave the end bit clear and grow the duration by 160. Three packets set the end bit, with the same timestamp and the final duration. `ReadDTMF` returns event 5 once. `Done` closes when the BYE completes.

The other pcap never sets the end bit. Three event packets share a timestamp, then one PCMU packet uses a later timestamp. `ReadDTMF` still returns 5 once, and `Done` closes on BYE.

`play_pcap_audio` sends that RTP through a raw UDP socket. `sipp` needs `CAP_NET_RAW`:

```bash
sudo setcap cap_net_raw+ep "$(command -v sipp)"
```

Ubuntu installs the binary as `sip-tester`. CI installs that package and sets the capability before `go test`. Without the capability the UAC test fails, because SIPp cannot open the socket. The test does not skip in that case.

## UAS

[uas.xml](../../transport/sip/testdata/sipp/uas.xml) answers one INVITE. The test reads `c=` from the INVITE in SIPp's message log.

| Mode | Bind | What `c=` must be |
| --- | --- | --- |
| `behind-sbc` | `127.0.0.1` | `Addresses.LocalIP` |
| `standalone` | wildcard, `AdvertiseIP` `127.0.0.2` | `Addresses.AdvertiseIP` |

`127.0.0.2` is loopback, so a response sent to the advertised Via still reaches the wildcard SIP socket. It is not the address `Start` picks as the local route, so the two checks are different addresses.
