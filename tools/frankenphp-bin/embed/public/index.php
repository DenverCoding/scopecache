<?php
// Minimal FrankenPHP + scopecache demo.
//
// This page does three things:
//   1. Try to read item id="greeting" from scope "demo" via scopecache.
//   2. If it's missing (cold cache, or after a container restart), upsert
//      a greeting + the current timestamp.
//   3. Display the message and whether the read was a hit or a miss.
//
// PHP talks to scopecache over loopback HTTP on the same :8080 listener —
// the scopecache module and php_server share one Caddy process, but PHP
// still goes out through the TCP socket to reach the cache.

const SC_BASE = 'http://localhost:8080';

function sc_get(string $scope, string $id): ?array {
    $url = SC_BASE . '/get?scope=' . rawurlencode($scope) . '&id=' . rawurlencode($id);
    $ch = curl_init($url);
    curl_setopt_array($ch, [
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_TIMEOUT        => 2,
    ]);
    $body = curl_exec($ch);
    if ($body === false) {
        return null;
    }
    $data = json_decode($body, true);
    if (!is_array($data) || empty($data['hit'])) {
        return null;
    }
    return $data['item'] ?? null;
}

function sc_upsert(string $scope, string $id, $payload): void {
    $body = json_encode([
        'scope'   => $scope,
        'id'      => $id,
        'payload' => $payload,
    ]);
    $ch = curl_init(SC_BASE . '/upsert');
    curl_setopt_array($ch, [
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_TIMEOUT        => 2,
        CURLOPT_POST           => true,
        CURLOPT_HTTPHEADER     => ['Content-Type: application/json'],
        CURLOPT_POSTFIELDS     => $body,
    ]);
    curl_exec($ch);
}

$item   = sc_get('demo', 'greeting');
$wasHit = $item !== null;

if (!$wasHit) {
    sc_upsert('demo', 'greeting', [
        'text'    => 'Hallo vanuit scopecache!',
        'created' => date('c'),
    ]);
    $item = sc_get('demo', 'greeting');
}

$payload = $item['payload'] ?? ['text' => '(geen)', 'created' => '?'];
?>
<!doctype html>
<html lang="nl">
<head>
    <meta charset="utf-8">
    <title>FrankenPHP + scopecache</title>
    <style>
        body { font-family: system-ui, sans-serif; max-width: 40em; margin: 2em auto; padding: 0 1em; }
        code { background: #f3f3f3; padding: 0.1em 0.3em; border-radius: 3px; }
    </style>
</head>
<body>
    <h1>Hallo wereld</h1>

    <p>Bericht uit scopecache:
        <strong><?= htmlspecialchars($payload['text']) ?></strong></p>

    <p>Voor het eerst opgeslagen op:
        <code><?= htmlspecialchars($payload['created']) ?></code></p>

    <p>Eerste lezing van deze request:
        <strong><?= $wasHit ? 'hit' : 'miss (net opgeslagen)' ?></strong></p>

    <p>Refresh de pagina — <code>created</code> verandert niet, want het
        bericht zit nu in de cache. <code>docker compose restart</code>
        of een call naar <code>/wipe</code> reset het. De cache leeft
        alleen in het geheugen van het proces.</p>

    <h2>Direct met de cache praten</h2>
    <ul>
        <li><a href="/stats">/stats</a> — overzicht van de cache</li>
        <li><a href="/tail?scope=demo&amp;limit=10">/tail?scope=demo</a> — laatste items in scope <code>demo</code></li>
        <li><a href="/get?scope=demo&amp;id=greeting">/get?scope=demo&amp;id=greeting</a> — ruwe JSON-respons</li>
    </ul>
</body>
</html>
