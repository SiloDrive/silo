# Putting Silo on the internet

Silo binds `127.0.0.1` and speaks plaintext. It has no TLS of its own and is
not going to grow any: terminating TLS is the reverse proxy's job, and a proxy
does it better — it reloads a renewed certificate without restarting the file
server. Every credential Silo uses is a bearer token in a header, so publishing
its port without a proxy in front hands those tokens to anyone on the path.

This page is the checklist for the day the port opens. The short version:

- [ ] Silo stays on loopback; the proxy is the only thing that can reach it.
- [ ] `SILO_TRUST_PROXY_HEADERS=true`, or every rate limiter counts nothing.
- [ ] `SILO_TRUSTED_PROXY_HOPS` matches the number of proxies in front — one
      unless you put something in front of your proxy.
- [ ] `/debug/pprof` blocked at the proxy.
- [ ] The setup token claimed before the port opens.
- [ ] Logs shipped off the box only after that.

## The rate limiters need the proxy headers, and nothing will tell you

Silo counts failed logins per address and per account. Behind a proxy every
request arrives from the proxy, so without `SILO_TRUST_PROXY_HEADERS=true`
every client on the server shares one bucket: ten failures from anywhere and
everybody is throttled, which is one attacker locking out the whole install.

The trap is that the warning saying so is attached to the wrong condition. It
is printed by `warnIfExposedWithoutTLS`, which returns early when the listen
address is loopback — and loopback is the correct configuration. So the
operator who has done everything right is the one who never sees it.

```sh
SILO_TRUST_PROXY_HEADERS=true
```

Set it whenever anything proxies to Silo. Do not set it when Silo is reachable
directly, because then the header is written by whoever is talking to it.

### How many proxies

`X-Forwarded-For` is a path, not a value. Each hop appends the address it saw,
so the list runs oldest first and the entries at the front were written by
whoever was furthest out — which, on a request through one proxy, means the
client. Silo counts from the right, and `SILO_TRUSTED_PROXY_HOPS` says how far
in to count.

One is the default and covers Caddy or nginx in front of Silo and nothing else.
Two is Cloudflare in front of Caddy. Getting it wrong in one direction is safe
and the other is not: too low attributes requests to the proxy nearest the
client, which groups more clients into a bucket than it should, and too high
reads an entry the client wrote and hands them a rate-limit key they choose.
So it never grows on its own — say it out loud or you get one.

## Caddy

```caddyfile
silo.example.com {
    reverse_proxy 127.0.0.1:8082 {
        # Uploads and downloads are whole files; do not let the proxy buffer
        # them, and do not let it cap them.
        flush_interval -1
    }

    # Profiling is off by default and these routes still answer. Blocking them
    # here costs nothing and does not depend on the config staying that way.
    @pprof path /debug/pprof*
    respond @pprof 404

    header {
        Strict-Transport-Security "max-age=31536000; includeSubDomains"
        X-Content-Type-Options nosniff
        X-Frame-Options DENY
        Referrer-Policy no-referrer
        -Server
    }
}
```

Caddy appends the client address to `X-Forwarded-For` on its own, which is what
Silo expects. Do not add `trusted_proxies` unless something really is in front
of Caddy, and if you do, set `SILO_TRUSTED_PROXY_HOPS` to match.

**No `Content-Security-Policy` at the proxy.** The admin page sets its own, and
a header set here would overwrite it with something laxer.

## nginx

```nginx
server {
    listen 443 ssl;
    server_name silo.example.com;
    ssl_certificate     /etc/letsencrypt/live/silo.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/silo.example.com/privkey.pem;

    # Uploads are whole files; do not let the proxy cap them.
    client_max_body_size 0;
    proxy_request_buffering off;

    location /debug/pprof {
        return 404;
    }

    location / {
        proxy_pass http://127.0.0.1:8082;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

`$proxy_add_x_forwarded_for` appends, which is correct: it is
`$http_x_forwarded_for, $remote_addr`, so the entry nginx wrote is the last one
and that is the one Silo reads.

## Keep Silo on loopback

`SILO_HOST` defaults to `127.0.0.1` and should stay there. Setting it to
anything else logs a warning on every start, because from inside the process
Silo cannot tell whether a proxy is there.

Docker is the exception that catches people, because the container's `EXPOSE`
and the host's port mapping are different decisions and only the second one is
yours:

```yaml
ports:
  - "127.0.0.1:8082:8082"   # not "8082:8082"
```

`"8082:8082"` publishes the container's port on every host interface, in front
of any firewall rule you wrote for the host, and Silo is then answering the
internet in plaintext.

## Claim the setup token first

A server that has never had an account mints a setup token and reprints it to
the log on every boot until somebody claims it. That is deliberate — an
operator who scrolled past it should not have to wonder which of two strings is
live — but it means an unclaimed server is publishing an account-bootstrap
credential to its log file on a timer.

Claim it before the port opens, and before any log shipping is switched on:

```sh
silo setup-token      # reprints the live one
```

## Profiling

`enable_profiling` is off by default. Leave it off. When it is on, the password
travels in a query string, which puts it in the proxy's access log — so if you
ever need it, turn it on for the duration and off again, and block the routes
at the proxy regardless, as above.

## What the proxy cannot do for you

Silo is invite-only, and an invite is the only way an account comes into being.
Sharing a library to an address that has no account creates a row for it. So
"anybody with an account" is exactly "anybody you or your users invited", and
the blast radius of a careless invite is the whole authenticated surface. That
is a policy question rather than a configuration one, and no header sets it.
