# G.711 and resample

Owners: JAR-5 for codec vectors, JAR-6 for the rate tests.

PCMU and PCMA stay at 8000 Hz. `ReadPCM` returns 160 samples, 320 bytes, 16-bit little-endian, at that clock. `WritePCM` takes the same shape. The wire clock is the negotiated codec clock. Both codecs are 8000, so the Call PCM API is 8000 in this build.

Use jargo v0.6.1, not a fork:

- `github.com/gojargo/jargo/audio/g711` `EncodeULaw`, `DecodeULaw`, `EncodeALaw`, `DecodeALaw`. On this tag each returns `[]byte`.
- `github.com/gojargo/jargo/audio` `ULawToPCM`, `PCMToULaw`, `ALawToPCM`, `PCMToALaw`. A nil resampler leaves the rate alone. Decode, then resample. Resample, then encode.

JAR-5 checks our payloads against those functions on the same PCM vectors. Silence on the wire is µ-law `0xFF` and A-law `0xD5` for every byte of a 160-byte payload.

320 bytes of PCM become one 160-byte payload. That is the JAR-6 size check.

## One resample, and only when the rates differ

The pipeline rate is the transport parameter ([pipeline-adapter.md](pipeline-adapter.md)). The default is 8000 in and 8000 out. When the pipeline rate equals 8000, no resampler is built. The conversion is a copy. JAR-6: an 8000 pipeline builds no resampler.

When the rates differ, build one `audio/resample` `Resampler` per direction with `resample.NewWithConfig` and `ClearAfter: resample.NeverClear`. `NeverClear` is negative. A positive idle window is not used. There is no waypoint at 16000 or 24000. A negotiated 8000 Hz clock above a lower pipeline rate is downsampled in that one stream. A pipeline rate above 8000 is downsampled to 8000 on the way to the wire, and 8000 is upsampled to the pipeline rate on the way in.

JAR-6: a 16000 pipeline builds one resampler per direction that is running, and the RTP payload is still 160 bytes.

Equal clocks on a `Resampler` are a passthrough inside jargo (`inRate == outRate`). This build does not construct that object when the rates are equal, so the "no resampler" check can see a nil stream.

The Call methods stay at 8000. The resample sits at the pipeline edge, which JAR-6 tests directly so the adapter story can rely on it. `Resampler.Clear` runs when the remote address changes, together with the jitter reset. `Flush` is not used to invent samples at the end of a call.
