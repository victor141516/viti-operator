import { execFileSync, spawn } from "node:child_process";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const browserPath = process.env.VITI_BROWSER_PATH || [
  "C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
  "C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe",
  "C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe",
].find(existsSync);
if (!browserPath) throw new Error("No Chromium browser was found. Set VITI_BROWSER_PATH.");
const pageURL = process.env.VITI_WEB_URL || "http://127.0.0.1:18788/?testTone=1&testDisconnect=1#calls";
const repoRoot = new URL("..", import.meta.url).pathname.replace(/^\/(.:)/, "$1").replace(/\//g, "\\");
const composeRoot = process.env.VITI_COMPOSE_ROOT || repoRoot;
const recoveryRounds = Number(process.env.VITI_RECOVERY_ROUNDS || "2");
const delay = milliseconds => new Promise(resolve => setTimeout(resolve, milliseconds));

async function waitFor(label, probe, timeoutMilliseconds = 30000) {
  const deadline = Date.now() + timeoutMilliseconds;
  while (Date.now() < deadline) {
    try {
      const value = await probe();
      if (value) return value;
    } catch {
      // A navigation or SIP state transition may briefly invalidate the page context.
    }
    await delay(200);
  }
  throw new Error(`${label} timed out`);
}

class BrowserClient {
  constructor(name, port) {
    this.name = name;
    this.port = port;
    this.profile = mkdtempSync(join(tmpdir(), `viti-conference-${name}-`));
    this.process = spawn(browserPath, [
      "--headless", "--disable-gpu", "--no-sandbox", "--no-first-run", "--no-default-browser-check",
      "--autoplay-policy=no-user-gesture-required", "--use-fake-ui-for-media-stream",
      "--use-fake-device-for-media-stream", "--remote-debugging-address=127.0.0.1",
      `--remote-debugging-port=${port}`, `--user-data-dir=${this.profile}`, "about:blank",
    ], { stdio: "ignore" });
    this.pending = new Map();
    this.requestID = 0;
  }

  async connect() {
    await waitFor(`${this.name} DevTools`, async () => (await fetch(`http://127.0.0.1:${this.port}/json/version`).catch(() => null))?.ok);
    const response = await fetch(`http://127.0.0.1:${this.port}/json/new?${encodeURIComponent(pageURL)}`, { method: "PUT" });
    if (!response.ok) throw new Error(`${this.name}: could not open page (${response.status})`);
    const page = await response.json();
    this.socket = new WebSocket(page.webSocketDebuggerUrl);
    await new Promise((resolve, reject) => {
      this.socket.addEventListener("open", resolve, { once: true });
      this.socket.addEventListener("error", reject, { once: true });
    });
    this.socket.addEventListener("message", ({ data }) => {
      const message = JSON.parse(data);
      if (!message.id || !this.pending.has(message.id)) return;
      const { resolve, reject } = this.pending.get(message.id);
      this.pending.delete(message.id);
      if (message.error) reject(new Error(message.error.message));
      else resolve(message.result);
    });
    await this.command("Runtime.enable");
    await waitFor(`${this.name} web application`, () => this.evaluate(
      "location.protocol.startsWith('http') && document.readyState === 'complete' && typeof state !== 'undefined'",
    ), 45000);
    await this.evaluate("sessionStorage.setItem('viti-e2e-test-tone', '1'); true");
  }

  command(method, params = {}) {
    const id = ++this.requestID;
    this.socket.send(JSON.stringify({ id, method, params }));
    return new Promise((resolve, reject) => this.pending.set(id, { resolve, reject }));
  }

  async evaluate(expression) {
    const result = await this.command("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
    if (result.exceptionDetails) {
      const detail = result.exceptionDetails.exception?.description
        || result.exceptionDetails.text
        || "unknown JavaScript exception";
      throw new Error(`${this.name}: browser expression failed: ${detail}`);
    }
    return result.result?.value;
  }

  async close() {
    try { await this.command("Browser.close"); } catch { this.socket?.close(); }
    await delay(300);
    if (!this.process.killed) this.process.kill();
    rmSync(this.profile, { recursive: true, force: true });
  }
}

// Keep these outside Windows/Hyper-V's commonly reserved 9xxx ranges.
const first = new BrowserClient("first", 12051);
const second = new BrowserClient("second", 12052);
const callsFetch = `fetch('/api/v1/calls').then(response => response.json()).then(body => body.calls)`;
const asterisk = command => execFileSync("docker", ["compose", "exec", "-T", "engine", "asterisk", "-rx", command], {
  cwd: composeRoot,
  encoding: "utf8",
});
const conferencePartyCount = () => {
  const match = asterisk("confbridge list").match(/^\S+\s+(\d+)\s+/m);
  return match ? Number(match[1]) : 0;
};

let result;
try {
  await Promise.all([first.connect(), second.connect()]);
  await Promise.all([first, second].map(client => waitFor(`${client.name} registration`, () => client.evaluate(
    "document.readyState === 'complete' && typeof state !== 'undefined' && state.voiceRegistered === true",
  ), 45000)));
  const openCalls = await first.evaluate(`${callsFetch}.then(calls => calls.filter(call => !call.ended_at).length)`);
  if (openCalls) throw new Error("A call was already active; conference test was not started");

  asterisk("channel originate Local/inbound@viti-diagnostic application Echo");
  await Promise.all([first, second].map(client => waitFor(`${client.name} incoming call`, () => client.evaluate("state.incomingCall === true"), 30000)));

  await first.evaluate("document.querySelector('#answer-call').click(); true");
  await waitFor("first participant active with media", () => first.evaluate(
    "state.callEstablished === true && document.querySelector('#call-status').textContent.includes('two-way audio')",
  ), 30000);
  await waitFor("second participant join offer", () => second.evaluate(
    "state.localCall === false && !document.querySelector('#join-call').hidden && document.querySelector('#join-call').textContent === 'Join'",
  ), 30000);

  await second.evaluate("document.querySelector('#join-call').click(); true");
  await waitFor("second participant active with media", () => second.evaluate(
    "state.callEstablished === true && document.querySelector('#call-status').textContent.includes('two-way audio')",
  ), 30000);
  const threeParty = /^\S+\s+3\s+/m.test(asterisk("confbridge list"));

  await first.evaluate("document.querySelector('#mute-call').click(); true");
  await waitFor("first participant muted", () => first.evaluate("state.microphoneMuted === true"), 10000);
  let repeatedRecovery = true;
  for (let round = 0; round < recoveryRounds; round += 1) {
    await first.evaluate("state.voice.interruptForTest(); true");
    await waitFor(`first participant media loss ${round + 1}`, () => Promise.resolve(conferencePartyCount() === 2), 15000);
    const retained = await second.evaluate(`${callsFetch}.then(calls => calls.some(call => !call.ended_at && call.state === 'active'))`);
    const recovered = await waitFor(`first participant recovery ${round + 1}`, () => first.evaluate(
      "state.callEstablished === true && state.microphoneMuted === true",
    ), 30000);
    await waitFor(`three-party conference restored ${round + 1}`, () => Promise.resolve(conferencePartyCount() === 3), 30000);
    repeatedRecovery = repeatedRecovery && retained && recovered;
  }

  await first.evaluate("document.querySelector('#hangup-call').click(); true");
  try {
    await waitFor("first participant left", () => first.evaluate("state.localCall === false"), 15000);
  } catch (error) {
    const snapshot = await first.evaluate(`(() => {
      let context = null;
      try { context = state.voice.callContext(); } catch {}
      return {
        status: document.querySelector('#call-status').textContent,
        context,
        recoveryContext: state.recoveryContext,
        callWasActive: state.callWasActive,
      };
    })()`);
    throw new Error(`${error.message}: ${JSON.stringify(snapshot)}`);
  }
  const secondSurvived = await second.evaluate("state.localCall === true && state.callEstablished === true");
  const callStillActive = await second.evaluate(`${callsFetch}.then(calls => calls.some(call => !call.ended_at && call.state === 'active'))`);
  const twoParty = /^\S+\s+2\s+/m.test(asterisk("confbridge list"));

  await second.evaluate(`(() => {
    window.__vitiE2EReconnects = [];
    const reconnect = state.voice.reconnect.bind(state.voice);
    state.voice.reconnect = (...args) => {
      window.__vitiE2EReconnects.push(args);
      return reconnect(...args);
    };
    return true;
  })()`);
  await second.evaluate("state.voice.interruptForTest(); true");
  await waitFor("all local media disconnected", () => Promise.resolve(conferencePartyCount() === 1), 60000);
  const retainedWithoutClients = await first.evaluate(`${callsFetch}.then(calls => calls.some(call => !call.ended_at && call.state === 'active'))`);
  let recoveredWithoutPeer;
  try {
    recoveredWithoutPeer = await waitFor("last logical participant recovery", () => second.evaluate(
      "state.callEstablished === true",
    ), 60000);
  } catch (error) {
    const snapshot = await second.evaluate(`({
      localCall: state.localCall,
      callEstablished: state.callEstablished,
      callWasActive: state.callWasActive,
      voiceRegistered: state.voiceRegistered,
      recoveryContext: state.recoveryContext,
      reconnects: window.__vitiE2EReconnects,
      termination: state.voice.terminationForTest(),
      endingExplicitly: state.endingExplicitly,
      status: document.querySelector('#call-status').textContent,
    })`);
    throw new Error(`${error.message}: ${JSON.stringify(snapshot)}`);
  }
  await waitFor("two-party conference restored", () => Promise.resolve(conferencePartyCount() === 2), 30000);

  await second.evaluate("document.querySelector('#hangup-call').click(); true");
  await waitFor("last participant left", () => second.evaluate("state.localCall === false"), 15000);
  await waitFor("carrier conference ended", () => Promise.resolve(!/^\S+\s+\d+\s+/m.test(asterisk("confbridge list"))), 15000);
  const historyFinalized = await waitFor("call history finalized", () => second.evaluate(
    `${callsFetch}.then(calls => calls.some(call => call.peer === 'Prueba local' && Boolean(call.ended_at)))`,
  ), 20000);

  result = {
    three_party_mix: threeParty,
    repeated_recovery_preserved_identity_and_mute: repeatedRecovery,
    first_left_independently: secondSurvived && callStillActive && twoParty,
    carrier_retained_without_client_media: retainedWithoutClients && recoveredWithoutPeer,
    last_left_ended_carrier: Boolean(historyFinalized),
  };
  console.log(JSON.stringify(result));
  if (!Object.values(result).every(Boolean)) process.exitCode = 1;
} finally {
  try {
    const conferences = [...asterisk("confbridge list").matchAll(/^(\S+)\s+\d+\s+/gm)].map(match => match[1]);
    for (const conference of conferences) asterisk(`confbridge kick ${conference} all`);
  } catch {}
  await Promise.allSettled([first.close(), second.close()]);
}
