# Remote deployment (Linux, e.g. a home server)

This is the runbook for running accountant on a second machine — a Linux
box on your LAN, with **optional** secure exposure to the internet via a
Cloudflare named tunnel + Access. Nothing here touches your primary
machine's setup.

Threat model, stated plainly:

- By default accountant binds `127.0.0.1` only. Local attackers are the
  perimeter; don't expose the port directly.
- Credentials on Linux live in a **file vault**: a `0700` directory of
  `0600` files under `~/.local/state/accountant/vault`. Permissions are
  enforced, not assumed — a loose file is refused, not served. On macOS
  the Keychain backend is used instead; nothing is shared between the
  two machines.
- If you expose accountant remotely, it must only ever be through the
  Cloudflare tunnel with a Cloudflare Access **service token** in front
  (Part B). The proxy requires its client key too, so an attacker needs
  both.

## Part A — core deploy (no exposure)

### 1. Go toolchain (user-scoped, no root)

```sh
curl -fsSL https://go.dev/dl/go1.27.2.linux-amd64.tar.gz -o /tmp/go.tgz
mkdir -p ~/.local && tar -C ~/.local -xzf /tmp/go.tgz && rm /tmp/go.tgz
echo 'fish_add_path ~/.local/go/bin'   # or add to PATH in your shell rc
```

(`apt install golang-go` works too; any recent Go is fine.)

### 2. Build and install

```sh
git clone <repo> ~/src/accountant && cd ~/src/accountant
go build -o ~/.local/bin/accountant ./cmd/accountant
```

### 3. Configuration

Create `~/.config/accountant/config.toml`:

```toml
listen = "127.0.0.1:8787"
default_provider = "anthropic"
# vault_backend defaults to "auto": file vault on Linux, Keychain on macOS.
# vault_dir defaults to ~/.local/state/accountant/vault.
vault_backend = "file"

[[accounts]]
id = "primary"
provider = "anthropic"
priority = 10
```

Fix permissions — `doctor` will nag until you do, and the vault refuses
to run otherwise:

```sh
chmod 700 ~/.config/accountant ~/.local/state/accountant
chmod 600 ~/.config/accountant/config.toml
```

### 4. Clients and accounts

```sh
accountant client add pi          # client key used by remote callers
accountant account add            # paste a Claude setup token
accountant account add --from ~/.codex/auth.json codex-work   # if needed
accountant doctor                 # everything should PASS
```

### 5. systemd user service

```sh
mkdir -p ~/.config/systemd/user
cp ~/src/accountant/integrations/remote/accountant.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now accountant
journalctl --user-unit accountant -f      # watch it start
```

Verify locally:

```sh
curl -s -H "Authorization: Bearer <client-key>" http://127.0.0.1:8787/v1/models | head
```

## Part B — optional: secure exposure (Cloudflare tunnel + Access)

> Do this only if remote clients (Amp, a laptop off-LAN) must reach this
> box. It puts a public hostname in front of accountant — Access and the
> client key are the two locks on that door.

Prereqs: a Cloudflare-managed domain and `cloudflared`:

```sh
# cloudflared from Cloudflare's repo (or the .deb from GitHub releases)
curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg | sudo tee /usr/share/keyrings/cloudflare-main.gpg >/dev/null
echo 'deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main' | sudo tee /etc/apt/sources.list.d/cloudflared.list
sudo apt update && sudo apt install cloudflared
```

1. **Authenticate once**: `cloudflared tunnel login` (pick the zone).
2. **Create the named tunnel** and route DNS:

   ```sh
   cloudflared tunnel create accountant-tunnel
   cloudflared tunnel route dns accountant-tunnel accountant.example.com
   ```

3. **Ingress config**: copy `integrations/remote/cloudflared-accountant.yml`
   to `~/.cloudflared/accountant.yml`, fill in the tunnel ID from step 2
   and your hostname. The trailing `- service: not_found` rule refuses
   every other hostname.
4. **Service**: copy `integrations/remote/cloudflared-accountant.service`
   to `~/.config/systemd/user/`, `daemon-reload`, `enable --now`.
5. **Cloudflare Access** (Zero Trust dashboard → Access → Applications):
   - Self-hosted app, domain `accountant.example.com`.
   - Policy: **Service Auth** → require a service token (create one).
   - Save the token's `Client ID` / `Client Secret`; they go in
     `CF_ACCESS_CLIENT_ID` / `CF_ACCESS_CLIENT_SECRET` on each client.
6. **Verify the locks**: without headers the tunnel must 403; with the
   service-token headers but no client key it must 401; with both, the
   proxy must serve.

## Part C — optional: Amp routers

With Part B in place, create the two Amp custom-url routers (Amp stores
connections in its account and requires HTTPS, hence the tunnel):

```sh
CF_ACCESS_CLIENT_ID=... CF_ACCESS_CLIENT_SECRET=... \
ACCOUNTANT_CLIENT_KEY=... \
node integrations/amp/amp-router.mjs --url https://accountant.example.com --personal
# looks right? add --apply
```

Amp priority vs existing routers is managed in Amp's own account UI.

## Part D — updates

```sh
cd ~/src/accountant && git pull
go build -o ~/.local/bin/accountant ./cmd/accountant
systemctl --user restart accountant
node integrations/update-catalog.mjs     # refresh local Pi extensions if used
```

## Troubleshooting

- **"must not be group/world accessible"** — the vault dir or a
  credential file is permissive. `chmod 700` the dir, `chmod 600` the
  files, or delete and re-add the credential.
- **`vault_backend "keychain" requires macOS`** — explicit keychain
  selection on Linux; drop the line to use `auto`.
- **Credential not found** — the file vault is per-machine; accounts must
  be re-added on this box (`accountant account add`). Config files can be
  copied; vaults cannot.
- **cloudflared 403 for everything** — Access policy not matching; check
  the service token and that the app domain equals the tunnel hostname.
