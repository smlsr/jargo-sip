# REGISTER

Owner: JAR-11. Depends on the dialog stack (JAR-8). A nil `Register` does not register. Direct trunks leave it nil and match INVITE sources against `AllowFrom`.

When `Register` is non-nil, we register the AoR `sip:User@Domain`. The request URI is `Register.Registrar` when that is set, otherwise `sip:Domain`. The Contact is the advertised contact from [config.md](config.md). `Call-ID` for the registration is stable across refreshes. CSeq increases by one each REGISTER.

Digest uses realm `Domain`. `Config.Auth` supplies the password, and the username is `Auth.Username` or `User`. One challenge is answered once, with `github.com/icholy/digest` v1.2.0 through sipgo's digest helper. A second `401` or `407` fails the registration. A challenge whose realm is not `Domain` fails the registration.

`Expires` is sent only when `Register.Expires` is non-zero. Otherwise the header is omitted and the registrar's expires is kept. We refresh at half the agreed lifetime, and we send `Expires: 0` on `Endpoint.Close`.

`Start` sends the first REGISTER after the UDP socket is up and the advertise address is known. A rejected REGISTER or a timeout is returned from `Start`. A later refresh failure is logged and retried once at the same interval. A second consecutive failure is logged and leaves the socket up so existing calls continue. `AllowFrom` still applies to INVITEs.

OPTIONS from the registrar are answered as in [options.md](options.md). REGISTER does not create a `Call` and does not allocate an RTP port.
