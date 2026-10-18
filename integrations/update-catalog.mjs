#!/usr/bin/env node
/**
 * update-catalog.mjs — refresh accountant's static model catalog.
 *
 * Fetches the live model lists from a running accountant daemon
 * (GET /v1/models and GET /codex/models), then:
 *
 *   1. writes integrations/catalog.json (raw upstream payloads, for any
 *      client integration to consume), and
 *   2. splices the same data into the STATIC CATALOG block of
 *      integrations/pi/accountant.ts (the pi extension is a single
 *      self-contained file, so its offline fallback is embedded).
 *
 * The mapping from raw payloads to client model configs lives in each
 * client integration; this script intentionally stores raw data only.
 *
 * Usage:
 *   node integrations/update-catalog.mjs [--url URL] [--client-name NAME]
 *                                        [--anthropic-file F --codex-file F]
 *                                        [--allow-partial] [--check]
 *
 * Auth: $ACCOUNTANT_CLIENT_KEY, else the login Keychain (service
 * "accountant-client", account NAME, default "pi").
 *
 * --check verifies the committed catalog matches a fresh fetch and exits
 * non-zero on drift (for release verification).
 */

import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = join(dirname(fileURLToPath(import.meta.url)), "..");
const EXTENSION = join(ROOT, "integrations", "pi", "accountant.ts");
const CATALOG_JSON = join(ROOT, "integrations", "catalog.json");
const BEGIN = "// === BEGIN STATIC CATALOG";
const END = "// === END STATIC CATALOG";

function parseArgs(argv) {
	const args = {
		url: process.env.ACCOUNTANT_URL ?? "http://127.0.0.1:8787",
		clientName: "pi",
		anthropicFile: undefined,
		codexFile: undefined,
		allowPartial: false,
		check: false,
	};
	for (let i = 0; i < argv.length; i++) {
		switch (argv[i]) {
			case "--url":
				args.url = argv[++i];
				break;
			case "--client-name":
				args.clientName = argv[++i];
				break;
			case "--anthropic-file":
				args.anthropicFile = argv[++i];
				break;
			case "--codex-file":
				args.codexFile = argv[++i];
				break;
			case "--allow-partial":
				args.allowPartial = true;
				break;
			case "--check":
				args.check = true;
				break;
			default:
				console.error(`unknown argument: ${argv[i]}`);
				process.exit(2);
		}
	}
	return args;
}

function clientKey(name) {
	if (process.env.ACCOUNTANT_CLIENT_KEY) {
		return process.env.ACCOUNTANT_CLIENT_KEY;
	}
	try {
		return execFileSync(
			"security",
			["find-generic-password", "-s", "accountant-client", "-a", name, "-w"],
			{ encoding: "utf8" },
		).trim();
	} catch {
		console.error(`error: no client key (set ACCOUNTANT_CLIENT_KEY or add a Keychain entry for "${name}")`);
		process.exit(1);
	}
}

async function fetchJson(url, headers) {
	const resp = await fetch(url, { headers });
	if (!resp.ok) {
		throw new Error(`GET ${url} -> HTTP ${resp.status}`);
	}
	return resp.json();
}

function diffIds(label, before, after) {
	const b = new Set(before);
	const a = new Set(after);
	const added = [...a].filter((x) => !b.has(x));
	const removed = [...b].filter((x) => !a.has(x));
	for (const x of added) console.log(`  ${label} + ${x}`);
	for (const x of removed) console.log(`  ${label} - ${x}`);
	return added.length + removed.length;
}

function currentStaticCatalog() {
	const src = readFileSync(EXTENSION, "utf8");
	const begin = src.indexOf(BEGIN);
	const end = src.indexOf(END);
	if (begin === -1 || end === -1 || end < begin) {
		console.error(`error: static catalog markers not found in ${EXTENSION}`);
		process.exit(1);
	}
	const block = src.slice(begin, src.indexOf("\n", end));
	const line = block.split("\n").find((l) => l.startsWith("const STATIC_CATALOG"));
	const m = line?.match(/= (\{.*\});$/);
	if (!m) {
		console.error("error: could not parse the embedded static catalog");
		process.exit(1);
	}
	return { src, begin, end, catalog: JSON.parse(m[1]) };
}

async function main() {
	const args = parseArgs(process.argv.slice(2));
	const previous = currentStaticCatalog().catalog;

	let anthropic;
	let codex;
	const key = args.anthropicFile && args.codexFile ? undefined : clientKey(args.clientName);
	// Each side comes from its --*-file when given, else from the live daemon.
	const [a, c] = await Promise.allSettled([
		args.anthropicFile
			? Promise.resolve(JSON.parse(readFileSync(args.anthropicFile, "utf8")))
			: fetchJson(`${args.url}/v1/models`, { "x-api-key": key, "anthropic-version": "2023-06-01" }),
		args.codexFile
			? Promise.resolve(JSON.parse(readFileSync(args.codexFile, "utf8")))
			: fetchJson(`${args.url}/codex/models`, { Authorization: `Bearer ${key}` }),
	]);
	if (a.status === "fulfilled") {
		anthropic = a.value.data ?? [];
	} else {
		console.error(`warning: anthropic fetch failed: ${a.reason.message}`);
	}
	if (c.status === "fulfilled") {
		codex = c.value.models ?? [];
	} else {
		console.error(`warning: codex fetch failed: ${c.reason.message}`);
	}

	if (anthropic === undefined && codex === undefined) {
		console.error("error: both fetches failed; keeping the committed catalog");
		process.exit(1);
	}
	if ((anthropic === undefined || codex === undefined) && !args.allowPartial) {
		console.error("error: partial catalog; re-run with --allow-partial to keep the missing side as-is");
		process.exit(1);
	}

	const catalog = {
		anthropic: anthropic ?? previous.anthropic ?? [],
		codex: codex ?? previous.codex ?? [],
		fetchedAt: new Date().toISOString(),
	};

	const changes =
		diffIds("anthropic", (previous.anthropic ?? []).map((m) => m.id), catalog.anthropic.map((m) => m.id)) +
		diffIds("codex", (previous.codex ?? []).map((m) => m.slug), catalog.codex.map((m) => m.slug));

	if (args.check) {
		if (changes > 0) {
			console.error("error: committed catalog is stale; run integrations/update-catalog.mjs");
			process.exit(1);
		}
		console.log("catalog is up to date");
		return;
	}

	writeFileSync(CATALOG_JSON, `${JSON.stringify(catalog, null, 2)}\n`);

	const { src, begin, end } = currentStaticCatalog();
	const endLine = src.indexOf("\n", end);
	const block =
		`${BEGIN} (generated by integrations/update-catalog.mjs; do not edit) ===\n` +
		`const STATIC_CATALOG: RawCatalog = ${JSON.stringify(catalog)};\n` +
		END + " ===";
	writeFileSync(EXTENSION, src.slice(0, begin) + block + src.slice(endLine));

	console.log(
		`catalog: ${catalog.anthropic.length} anthropic, ${catalog.codex.length} codex models ` +
			`(${changes} change${changes === 1 ? "" : "s"}); updated integrations/catalog.json and the pi extension`,
	);
}

main().catch((err) => {
	console.error(`error: ${err.message}`);
	process.exit(1);
});
