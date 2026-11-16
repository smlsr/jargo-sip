# SML Development receptionist

This example answers one inbound SIP call as the receptionist for SML Development, a custom builder. It greets the caller, listens, replies in short English sentences, offers to keep talking, and hangs up when the caller is done. It does not take a message.

The call sits on a jargo pipeline. `sip.NewTransport` is the pipeline transport. Caller audio goes in, a reply comes back out, and the conversation continues until the caller is finished.

## Speech

Cloud Speech-to-Text and Cloud Text-to-Speech reject `GEMINI_API_KEY` (they require OAuth). This example uses that same key on the Gemini API instead:

- Caller audio is recognized with Gemini audio-in on the dialog model.
- The dialog model is `gemini-3.5-flash`. The listener asks the key for its model list and keeps a Flash 3.5 model when the list has one. Otherwise it uses the newest text Flash model in that list.
- Replies are spoken with Gemini TTS model `gemini-3.8-flash-tts`.
- The voice is `en-gb-concierge-2`, a British female concierge voice. If that voice is rejected, the listener uses `en-au-concierge-3`, an Australian female concierge voice.

## Environment

Copy `.env.example` to `.env` and set the key. The process reads `GEMINI_API_KEY` from the environment, or from `.env` in this directory, or from `examples/receptionist/.env` when you start it from the repository root. The key is not compiled into the program.

| Variable | Default | Meaning |
| --- | --- | --- |
| `GEMINI_API_KEY` | | Gemini API key for recognition, dialog, and speech |
| `SIP_PORT` | `5060` | UDP port the listener binds |
| `SIP_LISTEN_IP` | all IPv4 | Address the SIP socket binds |
| `SIP_DOMAIN` | `sml-development.local` | SIP domain |
| `SIP_USER` | `receptionist` | SIP user |
| `SIP_DISPLAY_NAME` | `SML Development` | Name on the call |
| `SIP_ALLOW_FROM` | `0.0.0.0/0` | Comma-separated caller IPs or CIDRs |
| `SIP_ADVERTISE_IP` | | When set, the listener advertises this address (`standalone`). Leave it empty behind an SBC. |

## Start the listener

From the repository root:

```bash
go run ./examples/receptionist
```

Point the trunk at this host's UDP port. An allowed source that sends an INVITE is answered. The first thing the caller hears is a short greeting.

Audio on the trunk is 8 kHz G.711, PCMU then PCMA. The pipeline hears the caller at 16 kHz and speaks at 24 kHz. The SIP transport resamples both edges back to the 8 kHz trunk clock.
