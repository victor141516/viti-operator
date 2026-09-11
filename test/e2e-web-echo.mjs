import { execFileSync } from "node:child_process";
import { existsSync, writeFileSync } from "node:fs";

const cdpPort = process.env.VITI_CDP_PORT || "9231";
const pageURL = process.env.VITI_WEB_URL || "http://127.0.0.1:18788/?testTone=1#calls";
const holdMilliseconds = Math.max(0, Number(process.env.VITI_HOLD_MILLISECONDS || "0"));
const barrierPath = process.env.VITI_BARRIER_PATH || "";
const apiKey = process.env.VITI_TEST_API_KEY || "";
const testNumber = process.env.VITI_TEST_NUMBER || "*43";
const cdpBase = `http://127.0.0.1:${cdpPort}`;
const repoRoot = new URL("..", import.meta.url).pathname.replace(/^\/(.:)/, "$1").replace(/\//g, "\\");
const composeRoot = process.env.VITI_COMPOSE_ROOT || repoRoot;
const dtmfLogPath = `/tmp/viti-e2e-dtmf-${process.pid}`;

const composeExec = (...args) => execFileSync("docker", ["compose", "exec", "-T", "engine", ...args], {
  cwd: composeRoot,
  encoding: "utf8",
});
const asterisk = command => composeExec("asterisk", "-rx", command);
const asteriskLog = command => composeExec("sh", "-lc", command);

const delay = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

async function waitFor(label, probe, timeoutMilliseconds = 30000, diagnose = null) {
  const deadline = Date.now() + timeoutMilliseconds;
  let lastValue;
  while (Date.now() < deadline) {
    lastValue = await probe();
    if (lastValue) return lastValue;
    await delay(250);
  }
  let detail = "";
  if (diagnose) {
    try {
      detail = `: ${JSON.stringify(await diagnose())}`;
    } catch {
      detail = ": diagnostic snapshot unavailable";
    }
  }
  throw new Error(`${label} timed out${detail}`);
}

await waitFor("Chrome DevTools", async () => {
  try {
    const response = await fetch(`${cdpBase}/json/version`);
    return response.ok;
  } catch {
    return false;
  }
});

const pageResponse = await fetch(`${cdpBase}/json/new?${encodeURIComponent(pageURL)}`, {
  method: "PUT",
});
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
  const result = await command("Runtime.evaluate", {
    expression,
    awaitPromise: true,
    returnByValue: true,
  });
  if (result.exceptionDetails) throw new Error("browser expression failed");
  return result.result?.value;
}

let registered = false;
let active = false;
let bidirectional = false;
let dtmfStarted = false;
let dtmfStopped = false;
let dtmfReceived = false;
let historyFinalized = false;
let initialFinalizedLocalCalls = 0;
let dtmfLoggerEnabled = false;
let rtpDebugEnabled = false;
const callsFetch = `fetch('/api/v1/calls', {
  headers: localStorage.getItem('viti-token')
    ? { Authorization: 'Bearer ' + localStorage.getItem('viti-token') }
    : {}
}).then(response => {
  if (!response.ok) throw new Error('calls API returned ' + response.status);
  return response.json();
})`;
try {
  await command("Runtime.enable");
  await waitFor("web application", () => evaluate("document.readyState === 'complete' && typeof state !== 'undefined'"));
  await evaluate("sessionStorage.setItem('viti-e2e-test-tone', '1'); true");
  if (apiKey) {
    const previousTimeOrigin = await evaluate("performance.timeOrigin");
    await evaluate(`localStorage.setItem('viti-token', ${JSON.stringify(apiKey)}); location.reload(); true`);
    await waitFor("authenticated web application", async () => {
      try {
        return await evaluate(`document.readyState === 'complete' && typeof state !== 'undefined' && performance.timeOrigin !== ${previousTimeOrigin}`);
      } catch {
        return false;
      }
    });
  }
  registered = await waitFor("WebRTC registration", () => evaluate("state.voiceRegistered === true"), 30000);
  await waitFor(
    "call controls",
    () => evaluate(`
      state.voiceRegistered === true &&
      state.status?.ready === true &&
      state.globalCall === false &&
      document.querySelector('.call-button')?.disabled === false
    `),
    30000,
  );
  asteriskLog(`rm -f ${dtmfLogPath}`);
  asterisk(`logger add channel ${dtmfLogPath} verbose,dtmf`);
  dtmfLoggerEnabled = true;
  asterisk("rtp set debug on");
  rtpDebugEnabled = true;
  initialFinalizedLocalCalls = await evaluate(`${callsFetch}.then(value =>
    value.calls.filter(call => call.peer === ${JSON.stringify(testNumber)} && Boolean(call.ended_at)).length)`);
  await evaluate(`(() => {
    const input = document.querySelector('#call-to');
    input.value = ${JSON.stringify(testNumber)};
    input.dispatchEvent(new Event('input', { bubbles: true }));
    document.querySelector('#call-form').requestSubmit();
    return true;
  })()`);
  active = await waitFor(
    "local echo call",
    () => evaluate("state.localCall === true"),
    15000,
    () => evaluate(`({
      lineReady: state.status?.ready === true,
      voiceRegistered: state.voiceRegistered,
      localCall: state.localCall,
      incomingCall: state.incomingCall,
      globalCall: state.globalCall,
      dialPreparing: state.dialPreparing,
      callStatus: document.querySelector('#call-status')?.textContent || '',
      callButtonDisabled: document.querySelector('.call-button')?.disabled,
      destination: document.querySelector('#call-to')?.value || ''
    })`),
  );
  bidirectional = await waitFor(
    "bidirectional WebRTC media",
    () => evaluate("document.querySelector('#call-status').textContent.includes('two-way audio')"),
    30000,
  );
  dtmfStarted = await evaluate("state.voice.startDTMF('5') === true");
  await delay(240);
  dtmfStopped = await evaluate("state.voice.stopDTMF('5') === true");
  // Keep the call alive long enough for the RFC4733 end packets and
  // inter-tone gap queued by the browser to reach Asterisk.
  await delay(500);
  const newAsteriskLog = asteriskLog(`cat ${dtmfLogPath}`);
  dtmfReceived = /Got\s+RTP RFC2833[^\n]*event\s+00000005[^\n]*end\s+1/.test(newAsteriskLog)
    && /DTMF begin '5' received/.test(newAsteriskLog);
  if (barrierPath) {
    writeFileSync(`${barrierPath}.ready`, "ready\n", { encoding: "utf8", mode: 0o600 });
    await waitFor("external test release", () => existsSync(`${barrierPath}.release`), 300000);
  } else if (holdMilliseconds > 0) {
    await delay(holdMilliseconds);
  }
} finally {
  if (rtpDebugEnabled) {
    try { asterisk("rtp set debug off"); } catch {}
  }
  if (dtmfLoggerEnabled) {
    try { asterisk(`logger remove channel ${dtmfLogPath}`); } catch {}
    try { asteriskLog(`rm -f ${dtmfLogPath}`); } catch {}
  }
  try {
    await evaluate("document.querySelector('#hangup-call').click(); true");
    await waitFor("call hangup", () => evaluate("state.localCall === false"), 30000);
    historyFinalized = await waitFor(
      "finalized local call history",
      () => evaluate(`${callsFetch}.then(value =>
        value.calls.filter(call => call.peer === ${JSON.stringify(testNumber)} && Boolean(call.ended_at)).length > ${initialFinalizedLocalCalls})`),
      30000,
    );
  } catch {
    // The PowerShell wrapper also verifies and clears any remaining channel.
  }
  try {
    await command("Browser.close");
  } catch {
    socket.close();
  }
}

const result = {
  registered: Boolean(registered),
  active: Boolean(active),
  bidirectional: Boolean(bidirectional),
  dtmf_started: Boolean(dtmfStarted),
  dtmf_stopped: Boolean(dtmfStopped),
  dtmf_received: Boolean(dtmfReceived),
  history_finalized: Boolean(historyFinalized),
};
console.log(JSON.stringify(result));
if (!Object.values(result).every(Boolean)) process.exitCode = 1;
