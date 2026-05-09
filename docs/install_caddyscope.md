# Install Caddy + scopecache op een verse VPS

Op een verse Ubuntu/Debian-VPS is het één regel:

```bash
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/install_caddyscope.sh | sudo bash
```

Klaar — Caddy + scopecache draait, `/help` is getest, `wrk` is
geïnstalleerd.

Daarna de cache testen onder belasting:

```bash
wget https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/run_benchmark.sh
bash run_benchmark.sh
```

## Knoppen om aan te draaien

Allemaal optioneel als je de standaarden wil aanpassen — instellingen
gaan vóór `bash`:

```bash
# Andere poort
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/install_caddyscope.sh | sudo PORT=8080 bash

# Specifieke versie pinnen (in plaats van laatste)
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/install_caddyscope.sh | sudo VERSION=v0.8.18 bash

# Grotere capaciteit
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/install_caddyscope.sh | sudo MAX_STORE_MB=1024 SCOPE_MAX_ITEMS=1000000 bash

# Combineren
curl -fsSL https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/install_caddyscope.sh | sudo VERSION=v0.8.18 PORT=8080 MAX_STORE_MB=500 bash
```

## Twee-staps-vorm (als je het script eerst wil inzien)

`curl | sudo bash` voert iets uit dat je niet hebt ingezien. Voor een
script uit je eigen repo geen probleem, maar als je het script eerst
wil openen vóór je het draait:

```bash
wget https://raw.githubusercontent.com/VeloxCoding/scopecache/main/scripts/install_caddyscope.sh
# eventueel: cat install_caddyscope.sh   ← inzien
sudo bash install_caddyscope.sh
```

Instellingen-vorm:

```bash
sudo PORT=8080 VERSION=v0.8.18 bash install_caddyscope.sh
```
