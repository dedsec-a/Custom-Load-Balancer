# Deploying to AWS EC2

Cost: a t3.micro running 24/7 is about **$12/month** (instance ~$7.60 + public IPv4 ~$3.60 + disk under $1). $100 of credits covers roughly 8 months, if the credits don't expire first: check **Billing → Credits** for the expiry date.

```
Internet ──► :80/:443 Caddy (HTTPS) ──► lb ──► b1, b2, b3      (all Docker containers on one EC2 instance)
```

## 0. Billing safety (do this first, 2 minutes)

1. AWS console → **Billing and Cost Management → Budgets → Create budget** → "Monthly cost budget" → **$5** → your email.
2. Check **Billing → Credits** to see how much is left and when it expires.

## 1. Launch the instance

EC2 console → **Launch instance**:

| Setting | Value |
|---|---|
| Name | `loadbalancer-demo` |
| AMI | **Ubuntu Server 24.04 LTS** (64-bit x86) |
| Instance type | **t3.micro** |
| Key pair | Create new → RSA → `.pem` → save it, e.g. `C:\Users\<you>\.ssh\lb-demo.pem` |
| Network → Security group | Create: **SSH 22 from "My IP"**, **HTTP 80 from Anywhere**, **HTTPS 443 from Anywhere** |
| Storage | **16 GB gp3** (the Docker build cache needs room) |
| Advanced details → **Credit specification** | **Standard** (prevents surprise CPU charges under sustained load) |

Launch, then copy the instance's **Public IPv4 address**.

Optional but recommended: **Elastic IPs → Allocate → Associate** with the instance, so the address (and your URL) survives stop/start. It costs the same as the normal public IP.

## 2. Connect

From PowerShell on your PC:

```powershell
icacls $HOME\.ssh\lb-demo.pem /inheritance:r /grant:r "$($env:USERNAME):R"   # SSH refuses keys others can read
ssh -i $HOME\.ssh\lb-demo.pem ubuntu@<PUBLIC_IP>
```

## 3. Install Docker and add swap (on the server)

```bash
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker ubuntu && newgrp docker

# 2 GB swap so building the image fits in 1 GB of RAM
sudo fallocate -l 2G /swapfile && sudo chmod 600 /swapfile && sudo mkswap /swapfile && sudo swapon /swapfile
echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
```

## 4. Copy the code to the server

**Option A: GitHub (recommended; updates are one `git pull`)**
```bash
git clone https://github.com/<you>/<repo>.git loadbalancer && cd loadbalancer
```

**Option B: copy straight from your PC** (PowerShell, in the project folder)
```powershell
tar --exclude=node_modules --exclude=dist --exclude=bin --exclude=*.exe -czf lb.tgz Dockerfile docker-compose.yml .dockerignore .env.example deploy backend frontend
scp -i $HOME\.ssh\lb-demo.pem lb.tgz ubuntu@<PUBLIC_IP>:~
```
then on the server: `mkdir loadbalancer && tar -xzf lb.tgz -C loadbalancer && cd loadbalancer`

## 5. Configure and start

```bash
cp .env.example .env
nano .env    # set SITE_ADDRESS, see below
docker compose up -d --build     # first build takes ~3–6 min on a t3.micro
docker compose ps                # all 5 should be "Up", backends/lb "(healthy)"
```

**SITE_ADDRESS:** with no domain, use your public IP with dashes plus `.sslip.io`. For IP `13.233.10.20`, that's
`SITE_ADDRESS=13-233-10-20.sslip.io`. Caddy then gets a free HTTPS certificate automatically. Open **https://13-233-10-20.sslip.io**.

If you own a domain, point an A record at the IP and use it instead (`SITE_ADDRESS=lb.yourdomain.com`).
To skip HTTPS while testing, leave `SITE_ADDRESS` empty and open `http://<PUBLIC_IP>`.

## Everyday commands

```bash
docker compose logs -f lb        # watch load-balancer logs
docker compose logs caddy        # HTTPS certificate problems show up here
git pull && docker compose up -d --build   # deploy a new version (build on the server)
docker compose down              # stop everything (data-free, safe)
```

Containers restart automatically after a reboot (`restart: unless-stopped`).

## Safety built in (`--demo` mode in docker-compose.yml)

- At most 300 virtual users (`MAX_USERS` in `.env`), 8 cores and 50 DB connections per backend.
- Virtual users stop automatically after 10 minutes without a change.
- Adding and removing backends is disabled, so nobody can point the server at arbitrary URLs (SSRF).
- Backends and the admin port are never published; only Caddy (80/443) is.
- Optional login: set `LB_ADMIN_PASSWORD` in `.env` (user `admin`) and run `docker compose up -d` again.

## Saving money

Stopping the instance (EC2 → Instance state → **Stop**) stops the ~$7.60/month compute charge. You still pay for disk, plus the Elastic IP if you allocated one. **Terminate** the instance (and release the Elastic IP) when you're done for good.

## Updating without building on the server (small instances)

A t2/t3.micro has little disk and RAM, so it's easier to build on your PC and copy the images:

```powershell
# on your PC, in the project folder
docker compose build
docker save loadbalancer-lb:latest loadbalancer-b1:latest loadbalancer-b2:latest loadbalancer-b3:latest -o lbimages.tar
tar -czf lbimages.tar.gz lbimages.tar; Remove-Item lbimages.tar   # (or gzip it; docker load reads either)
scp -i <key.pem> lbimages.tar.gz ubuntu@<PUBLIC_IP>:~
```

```bash
# on the server
sha256sum ~/lbimages.tar.gz | cut -c1-16    # compare with the same command on your PC, so you know it arrived
cd ~/loadbalancer
docker load -i ~/lbimages.tar.gz             # reads the compressed file directly
docker compose up -d --no-build              # must say "Recreated" for b1, b2, b3, lb
docker compose exec caddy caddy reload --config /etc/caddy/Caddyfile   # only needed if the Caddyfile changed
docker image prune -f                        # remove the old versions
```

If `docker compose up` says only "Healthy" (no "Recreated"), the images didn't change: you probably loaded an old archive.

## Optional extras

- **Grafana:** `docker compose --profile observability up -d` adds Prometheus and Grafana, about 250 MB more RAM. Grafana listens on the server's localhost only. Open it through an SSH tunnel: `ssh -i key.pem -L 3000:localhost:3000 ubuntu@<IP>`, then visit http://localhost:3000.
- **Logs** are JSON, one event per line: `docker compose logs lb | grep '"backend ejected"'`.
- **Experiment results** are saved in the `experiments` volume and reload after restarts and deploys.
- **Your own domain:** point an A record at the instance IP and set `SITE_ADDRESS=lb.yourdomain.com` in `.env`, then `docker compose up -d` and reload Caddy.
