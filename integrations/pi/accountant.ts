/**
 * accountant provider for pi.
 *
 * Routes Anthropic traffic through the local accountant proxy
 * (http://127.0.0.1:8787), which rolls over between Claude subscriptions.
 * The model list comes from the proxied upstream GET /v1/models at startup
 * (cached in accountant-models-cache.json for when the daemon is down).
 *
 * The client key lives in the login Keychain (service "accountant-client",
 * account "pi"); create it with: accountant client add pi --claude-code-compat
 */

import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import type { ExtensionAPI, ProviderModelConfig } from "@earendil-works/pi-coding-agent";

const BASE_URL = "http://127.0.0.1:8787";
const CACHE = join(homedir(), ".pi", "agent", "accountant-models-cache.json");

interface UpstreamModel {
	id: string;
	display_name?: string;
}

function clientKey(): string | undefined {
	try {
		return (
			execFileSync("security", ["find-generic-password", "-s", "accountant-client", "-a", KEYCHAIN_ACCOUNT, "-w"], {
				encoding: "utf8",
			}).trim() || undefined
		);
	} catch {
		return undefined;
	}
}

function toModelConfig(m: UpstreamModel): ProviderModelConfig {
	return {
		id: m.id,
		name: m.display_name?.trim() || m.id,
		reasoning: true,
		input: ["text", "image"],
		// Subscription quota, not per-token billing: no meaningful cost.
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow: 200000,
		maxTokens: 64000,
	};
}

async function fetchModels(key: string, signal?: AbortSignal): Promise<ProviderModelConfig[]> {
	const resp = await fetch(`${BASE_URL}/v1/models`, {
		headers: { "x-api-key": key, "anthropic-version": "2023-06-01" },
		signal,
	});
	if (!resp.ok) {
		throw new Error(`accountant: GET /v1/models -> HTTP ${resp.status}`);
	}
	const data = (await resp.json()) as { data?: UpstreamModel[] };
	return (data.data ?? []).map(toModelConfig);
}

function cachedModels(): ProviderModelConfig[] {
	try {
		return JSON.parse(readFileSync(CACHE, "utf8")) as ProviderModelConfig[];
	} catch {
		return [];
	}
}

export default async function (pi: ExtensionAPI) {
	let models = cachedModels();
	const key = clientKey();
	if (key) {
		try {
			models = await fetchModels(key);
			writeFileSync(CACHE, JSON.stringify(models));
		} catch {
			// Daemon down: keep the cache.
		}
	}

	pi.registerProvider("accountant", {
		name: "accountant (subscription rollover)",
		baseUrl: BASE_URL,
		apiKey: "!security find-generic-password -s accountant-client -a pi -w",
		api: "anthropic-messages",
		models,
		refreshModels: async (context): Promise<ProviderModelConfig[]> => {
			const cred = context.credential;
			const k = (cred?.type === "api_key" ? cred.key : undefined) ?? clientKey();
			if (!k || !context.allowNetwork) {
				return cachedModels();
			}
			const fresh = await fetchModels(k, context.signal);
			writeFileSync(CACHE, JSON.stringify(fresh));
			return fresh;
		},
	});
}
