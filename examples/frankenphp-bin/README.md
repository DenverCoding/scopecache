# FrankenPHP + scopecache — standalone binary

One static Linux binary, ~33 MB. Bundles Caddy + FrankenPHP (PHP 8 ZTS)
+ scopecache + addons in a single x86_64 Linux ELF. No package installs,
no Docker required on the target, no shared libraries.

## Quick start on a VPS (Linux x86_64)

```bash
wget https://github.com/VeloxCoding/scopecache/raw/main/examples/frankenphp-bin/frankenphp-static-linux-x86_64
chmod +x frankenphp-static-linux-x86_64
./frankenphp-static-linux-x86_64 php-server
```

Open `http://<your-server>:8080/`. Each refresh appends a random word
to scope `demo` via PHP→cgo and shows the last 5 items.

## Quick start on Windows / macOS via Docker

```bash
docker run -d --name fpbin -p 8080:8080 \
    -v "$(pwd):/app:ro" \
    --entrypoint /app/frankenphp-static-linux-x86_64 \
    alpine:latest php-server
```

On Git-Bash for Windows prefix with `MSYS_NO_PATHCONV=1`. Stop with
`docker rm -f fpbin`.

## What you'll see

Open `/` in a browser. Each refresh:

1. Picks a random English word.
2. Calls `scopecache_append('demo', '', json_encode([...]))` — direct
   cgo into the in-process `*Gateway`, no HTTP roundtrip.
3. Calls `scopecache_tail('demo', 5)` and renders the last 5 items.
4. Shows per-call timings (typical warm: append ~25-30 µs, tail ~10-15 µs).

Other endpoints to hit directly:

- `/stats` — JSON snapshot of the whole cache
- `/tail?scope=demo&limit=10` — same items as the table, JSON envelope
- `/scopelist` — list of every scope (`_events`, `_inbox`, `demo`)
- `POST /wipe` — clear the cache without restarting

## Run as a service (systemd)

`/etc/systemd/system/scopecache.service`:

```ini
[Unit]
Description=FrankenPHP + scopecache
After=network.target

[Service]
Type=simple
ExecStart=/opt/scopecache/frankenphp-static-linux-x86_64 php-server
Restart=always
User=www-data

[Install]
WantedBy=multi-user.target
```

Then:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now scopecache
```

## Customising

The Caddyfile + PHP files are baked into the binary. To override:

```bash
./frankenphp-static-linux-x86_64 run --config /etc/my.Caddyfile
```

A minimal override Caddyfile:

```caddyfile
{
    auto_https off
    order scopecache before php_server
}

:8080 {
    root * /var/www/html
    scopecache {
        scope_max_items 10000
        max_store_mb    256
    }
    php_server
}
```

## Building from source

See [`tools/frankenphp-bin/`](../../tools/frankenphp-bin/) — `./build.sh`,
~15-45 min, needs Docker + a `GITHUB_TOKEN`.
