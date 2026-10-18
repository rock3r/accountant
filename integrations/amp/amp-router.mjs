#!/usr/bin/env node
/**
 * amp-router.mjs — generate Amp custom-url routers pointing at accountant.
 *
 * Amp (https://ampcode.com) stores model providers account-side. A
 * "custom url" router lets Amp reach any OpenAI-compatible or
 * Anthropic-compatible endpoint; accountant exposes both:
 *
 *   /codex      → ChatGPT/Codex subscriptions   (api-format: responses)
 *   /anthropic  → Claude subscriptions          (api-format: anthropic-messages)
 *
 * This script reads integrations/catalog.json (the raw model lists
 * fetched by update-catalog.mjs) and emits the two
 * `amp config model-providers add-router custom-url` commands with:
 *
 *   - an exact `provider/model -> model` mapping for every catalog
 *     model, so Amp's prefixed name (anthropic/claude-…, openai/gpt-…)
 *     is rewritten to the bare id accountant passes upstream, and
 *   - the Cloudflare Access service-token headers taken from the
 *     CF_ACCESS_CLIENT_ID / CF_ACCESS_CLIENT_SECRET environment
 *     variables (never stored in the repo).
 *
 * Usage:
 *   node integrations/amp/amp-router.mjs --url https://accountant.example.com \
 *          --personal [--rename old:new ...] [--activate] [--apply]
 *
 *   --url       the public HTTPS hostname fronting accountant
 *   --personal | --workspace   which Amp account the routers land in
 *   --rename    a:b renames model a (as Amp knows it) to b (as served);
 *               repeatable; identity mappings are still emitted for the rest
 *   --activate  activate the routers at creation (default: created
 *               inactive, so priority can be set before activation)
 *   --apply     run the commands instead of printing them (secrets are
 *               printed redacted otherwise)
 *
 * Amp only accepts HTTPS base URLs, so accountant must be fronted by a
 * tunnel (docs/remote-deploy.md). Auth to accountant itself uses the
 * client key: set ACCOUNTANT_CLIENT_KEY or it is read from the login
 * Keychain like in update-catalog.mjs.
 */

import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const here = path.dirname(fileURLToPath(import.meta.url));
const catalogPath = path.join(here, "..", "catalog.json");

function fail(msg) {
  console.error(`amp-router: ${msg}`);
  process.exit(1);
}

function parseArgs(argv) {
  const out = { renames: new Map(), apply: false, activate: false, scope: null, url: null };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--url") out.url = argv[++i];
    else if (a === "--personal") out.scope = a.slice(2);
    else if (a === "--workspace") out.scope = a.slice(2);
    else if (a === "--activate") out.activate = true;
    else if (a === "--apply") out.apply = true;
    else if (a === "--rename") {
      const [from, to] = (argv[++i] ?? "").split(":");
      if (!from || !to) fail(`--rename expects old:new, got ${argv[i]}`);
      out.renames.set(from, to);
    } else fail(`unknown argument ${a}`);
  }
  if (!out.url || !/^https:\/\//.test(out.url)) fail("--url must be an https:// URL");
  if (!out.scope) fail("one of --personal or --workspace is required");
  return out;
}

function clientKey() {
  if (process.env.ACCOUNTANT_CLIENT_KEY) return process.env.ACCOUNTANT_CLIENT_KEY;
  try {
    return execFileSync(
      "security",
      ["find-generic-password", "-s", "accountant-client", "-a", "pi", "-w"],
      { encoding: "utf8" },
    ).trim();
  } catch {
    fail("set ACCOUNTANT_CLIENT_KEY (no Keychain on this machine)");
  }
}

function accessHeaders() {
  const id = process.env.CF_ACCESS_CLIENT_ID;
  const secret = process.env.CF_ACCESS_CLIENT_SECRET;
  if (!id || !secret) {
    fail("CF_ACCESS_CLIENT_ID and CF_ACCESS_CLIENT_SECRET must be set (Cloudflare Access service token)");
  }
  // amp add-router takes headers as a JSON array of [key, value] pairs.
  return [
    ["CF-Access-Client-Id", id],
    ["CF-Access-Client-Secret", secret],
    ["Authorization", `Bearer ${clientKey()}`],
  ];
}

// Exact mapping for one provider's models. Amp knows models as
// `provider/model`; accountant passes the model string through to the
// upstream API, which wants the bare id — hence the -> override per
// model (Amp only allows overrides on exact ids, not patterns).
function modelMapping(models, renames, prefix, key) {
  const pairs = [];
  for (const m of models) {
    const id = m[key];
    pairs.push([`${prefix}/${renames.get(id) ?? id}`, id]);
  }
  return pairs;
}

const args = parseArgs(process.argv.slice(2));
const catalog = JSON.parse(readFileSync(catalogPath, "utf8"));
const headers = accessHeaders();

const routers = [
  {
    name: "accountant-codex",
    apiFormat: "responses",
    baseURL: `${args.url}/codex`,
    models: catalog.codex,
    key: "slug",
    prefix: "openai",
  },
  {
    name: "accountant-claude",
    apiFormat: "anthropic-messages",
    baseURL: `${args.url}/anthropic`,
    models: catalog.anthropic,
    key: "id",
    prefix: "anthropic",
  },
];

for (const r of routers) {
  const mapping = modelMapping(r.models, args.renames, r.prefix, r.key);
  const mappingArg = mapping.map(([a, b]) => `${a} -> ${b}`).join(", ");
  const cmd = [
    "amp", "config", "model-providers", "add-router", "custom-url",
    "--name", r.name,
    "--base-url", r.baseURL,
    "--api-format", r.apiFormat,
    "--model-mapping", mappingArg,
    "--headers", JSON.stringify(headers),
    args.activate ? "--active" : "--no-active",
    `--${args.scope}`,
  ];
  if (args.apply) {
    console.log(`applying router ${r.name} (${args.scope})...`);
    execFileSync("amp", cmd, { stdio: "inherit" });
  } else {
    const redacted = cmd.map((c) =>
      c
        .replace(/("CF-Access-Client-Secret","[^"]{4})[^"]*"/g, '$1…"')
        .replace(/("Authorization","Bearer [^"]{4})[^"]*"/g, '$1…"'),
    );
    console.log(redacted.join(" ") + "\n");
  }
}
if (!args.apply) {
  console.log("dry run: routers would be created INACTIVE (add --activate to flip them on at creation;");
  console.log("set priority with `amp config model-providers set-priority <id> <n>`, then `activate <id>`. Re-run with --apply to create them.)");
}
