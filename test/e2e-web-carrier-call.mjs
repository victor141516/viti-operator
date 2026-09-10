import { execFileSync } from "node:child_process";
import { existsSync, writeFileSync } from "node:fs";

const cdpPort = process.env.VITI_CDP_PORT || "9239";
const pageURL = process.env.VITI_WEB_URL || "http://127.0.0.1:18788/?testTone=1#calls";
const apiKey = process.env.VITI_TEST_API_KEY || "";
const destination = process.env.VITI_TEST_DESTINATION || "";
const barrier = process.env.VITI_BARRIER_PATH || "";
const requireRinging = process.env.VITI_REQUIRE_RINGING === "1";
const expectRemoteHangup = process.env.VITI_EXPECT_REMOTE_HANGUP === "1";
const cdpBase = `http://127.0.0.1:${cdpPort}`;
const repoRoot = new URL("..", import.meta.url).pathname.replace(/^\/(.:)/, "$1").replace(/\//g, "\\");
const composeRoot = process.env.VITI_COMPOSE_ROOT || repoRoot;

const composeExec = (...args) => execFileSync("docker", ["compose", "exec", "-T", "engine", ...args], {
  cwd: composeRoot,
  encoding: "utf8",
});
const asterisk = command => composeExec("asterisk", "-rx", command);
const asteriskLog = command => composeExec("sh", "-lc", command);

if (!/^\+[1-9][0-9]{7,14}$/.test(destination)) throw new Error("carrier destination is not E.164");
if (!barrier) throw new Error("VITI_BARRIER_PATH is required");

const delay = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));
async function waitFor(label, probe, timeoutMilliseconds = 30000) {
  const deadline = Date.now() + timeoutMilliseconds;
  while (Date.now() < deadline) {
    try {
      if (await probe()) return true;
    } catch {
      // A reload briefly invalidates the execution context.
    }
    await delay(250);
  }
  throw new Error(`${label} timed out`);
}

await waitFor("Chrome DevTools", async () => {
  try { return (await fetch(`${cdpBase}/json/version`)).ok; } catch { return false; }
});
const pageResponse = await fetch(`${cdpBase}/json/new?${encodeURIComponent(pageURL)}`, { method: "PUT" });
if (!pageResponse.ok) throw new Error(`could not open test page: HTTP ${pageResponse.status}`);
const page = await pageResponse.json();
const socket = new WebSocket(page.webSocketDebuggerUrl);
await new Promise((resolve, reject) => {
  socket.addEventListener("open", resolve, { once: true });
  socket.addEventListener("error", reject, { once: true });
});

let requestID = 0;
const pending = new Map();
socket.addEventListener("message", ({ data }) => {
  const message = JSON.parse(data);
  if (!message.id || !pending.has(message.id)) return;
  const { resolve, reject } = pending.get(message.id);
  pending.delete(message.id);
  if (message.error) reject(new Error(message.error.message));
  else resolve(message.result);
});
function command(method, params = {}) {
  const id = ++requestID;
  socket.send(JSON.stringify({ id, method, params }));
  return new Promise((resolve, reject) => pending.set(id, { resolve, reject }));
}
async function evaluate(expression) {
  const result = await command("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
  if (result.exceptionDetails) throw new Error("browser expression failed");
  return result.result?.value;
}

const callsFetch = `fetch('/api/v1/calls', {
  headers: localStorage.getItem('viti-token')
    ? { Authorization: 'Bearer ' + localStorage.getItem('viti-token') }
    : {}
}).then(response => response.json())`;
let registered = false;
let progressed = false;
let ringing = false;
let active = false;
let bidirectional = false;
let dtmfForwarded = false;
let remoteHungup = false;
let historyFinalized = false;
let initialFinalized = 0;
let asteriskLogStart = 0;
let rtpDebugEnabled = false;
try {
  await command("Runtime.enable");
  await waitFor("web application", () => evaluate("document.readyState === 'complete' && typeof state !== 'undefined'"));
  await evaluate("sessionStorage.setItem('viti-e2e-test-tone', '1'); true");
  if (apiKey) {
    const origin = await evaluate("performance.timeOrigin");
    await evaluate(`localStorage.setItem('viti-token', ${JSON.stringify(apiKey)}); location.reload(); true`);
    await waitFor("authenticated web application", () => evaluate(
      `document.readyState === 'complete' && typeof state !== 'undefined' && performance.timeOrigin !== ${origin}`,
    ));
  }
  registered = await waitFor("WebRTC registration", () => evaluate("state.voiceRegistered === true"), 35000);
  await waitFor("call controls", () => evaluate(`
    state.voiceRegistered === true &&
    state.status?.ready === true &&
    state.globalCall === false &&
    document.querySelector('.call-button')?.disabled === false
  `), 30000);
  asteriskLogStart = Number(asteriskLog("wc -l < /var/log/asterisk/full").trim());
  asterisk("rtp set debug on");
  rtpDebugEnabled = true;
  initialFinalized = await evaluate(`${callsFetch}.then(value => value.calls.filter(call =>
    call.peer === ${JSON.stringify(destination)} && Boolean(call.ended_at)).length)`);
  await evaluate(`(() => {
    const input = document.querySelector('#call-to');
    input.value = ${JSON.stringify(destination)};
    input.dispatchEvent(new Event('input', { bubbles: true }));
    document.querySelector('#call-form').requestSubmit();
    return true;
  })()`);
  await waitFor("carrier call start", () => evaluate("state.localCall === true"), 20000);
  progressed = await waitFor(
    "carrier call progress",
    () => evaluate(`
      state.localCall === true && (
        state.callEstablished === true ||
        document.querySelector('#call-status').textContent.includes('Ringing')
      )
    `),
    45000,
  );
  ringing = await evaluate("document.querySelector('#call-status').textContent.includes('Ringing')");
  writeFileSync(`${barrier}.web-ringing`, "ready\n", { encoding: "utf8", mode: 0o600 });
  await waitFor("carrier phone answer", () => existsSync(`${barrier}.phone-active`), 45000);
  active = await waitFor(
    "active carrier call",
    () => evaluate("state.localCall === true && state.callEstablished === true"),
    20000,
  );
  bidirectional = await waitFor(
    "bidirectional carrier media",
    () => evaluate("document.querySelector('#call-status').textContent.includes('two-way audio')"),
    45000,
  );
  await evaluate("state.voice.startDTMF('5'); true");
  await delay(240);
  await evaluate("state.voice.stopDTMF('5'); true");
  await delay(500);
  const newAsteriskLog = asteriskLog(`tail -n +${asteriskLogStart + 1} /var/log/asterisk/full`);
  dtmfForwarded = /Got\s+RTP RFC2833[^\n]*event\s+00000005[^\n]*end\s+1/.test(newAsteriskLog)
    && /Sent RTP DTMF packet to/.test(newAsteriskLog);
  if (expectRemoteHangup) {
    remoteHungup = await waitFor(
      "remote carrier hangup",
      () => evaluate("state.localCall === false"),
      90000,
    );
  } else {
    await delay(1500);
  }
} finally {
  if (rtpDebugEnabled) {
    try { asterisk("rtp set debug off"); } catch {}
  }
  try {
    if (!remoteHungup) {
      await evaluate("document.querySelector('#hangup-call').click(); true");
      await waitFor("call hangup", () => evaluate("state.localCall === false"), 30000);
    }
    historyFinalized = await waitFor("finalized carrier history", () => evaluate(`${callsFetch}.then(value =>
      value.calls.filter(call => call.peer === ${JSON.stringify(destination)} && Boolean(call.ended_at)).length > ${initialFinalized})`), 35000);
  } catch {
    // The wrapper performs channel cleanup and reports the failed invariant.
  }
  try { await command("Browser.close"); } catch { socket.close(); }
}

const result = {
  registered: Boolean(registered),
  progressed: Boolean(progressed),
  ringing: Boolean(ringing),
  active: Boolean(active),
  bidirectional: Boolean(bidirectional),
  dtmf_forwarded: Boolean(dtmfForwarded),
  remote_hangup: expectRemoteHangup ? Boolean(remoteHungup) : null,
  history_finalized: Boolean(historyFinalized),
};
console.log(JSON.stringify(result));
const required = result.registered && result.progressed && result.active
  && result.bidirectional && result.dtmf_forwarded && result.history_finalized;
if (!required || (requireRinging && !result.ringing) || (expectRemoteHangup && !result.remote_hangup)) process.exitCode = 1;
