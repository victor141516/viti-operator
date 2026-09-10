import { existsSync, writeFileSync } from "node:fs";

const cdpPort = process.env.VITI_CDP_PORT || "9241";
const pageURL = process.env.VITI_WEB_URL || "http://127.0.0.1:18788/?testTone=1#calls";
const barrierPath = process.env.VITI_BARRIER_PATH || "";
const apiKey = process.env.VITI_TEST_API_KEY || "";
const winner = process.env.VITI_INCOMING_WINNER === "android" ? "android" : "web";
const anyIncomingPeer = process.env.VITI_INCOMING_ANY_PEER === "1";
const expectedPeer = process.env.VITI_INCOMING_PEER || "Prueba local";
const cdpBase = `http://127.0.0.1:${cdpPort}`;

if (!barrierPath) throw new Error("VITI_BARRIER_PATH is required");

const delay = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

async function waitFor(label, probe, timeoutMilliseconds = 30000) {
  const deadline = Date.now() + timeoutMilliseconds;
  while (Date.now() < deadline) {
    try {
      const value = await probe();
      if (value) return value;
    } catch {
      // Navigation briefly invalidates the CDP execution context.
    }
    await delay(250);
  }
  throw new Error(`${label} timed out`);
}

const mark = (suffix) => writeFileSync(`${barrierPath}.${suffix}`, `${suffix}\n`, {
  encoding: "utf8",
  mode: 0o600,
});

await waitFor("Chrome DevTools", async () => {
  const response = await fetch(`${cdpBase}/json/version`).catch(() => null);
  return Boolean(response?.ok);
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
  const callbacks = pending.get(message.id);
  pending.delete(message.id);
  if (message.error) callbacks.reject(new Error(message.error.message));
  else callbacks.resolve(message.result);
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

const callsFetch = `fetch('/api/v1/calls', {
  headers: localStorage.getItem('viti-token')
    ? { Authorization: 'Bearer ' + localStorage.getItem('viti-token') }
    : {}
}).then(response => {
  if (!response.ok) throw new Error('calls API returned ' + response.status);
  return response.json();
})`;

let registered = false;
let ringing = false;
let active = false;
let bidirectional = false;
let joinedConference = false;
let historyFinalized = false;
let initialFinalizedCalls = 0;
const incomingCallFilter = anyIncomingPeer
  ? "call.direction === 'incoming' && Boolean(call.ended_at)"
  : `call.peer === ${JSON.stringify(expectedPeer)} && call.direction === 'incoming' && Boolean(call.ended_at)`;
try {
  await command("Runtime.enable");
  await waitFor("web application", () => evaluate("document.readyState === 'complete' && typeof state !== 'undefined'"));
  await evaluate("sessionStorage.setItem('viti-e2e-test-tone', '1'); true");
  if (apiKey) {
    const previousTimeOrigin = await evaluate("performance.timeOrigin");
    await evaluate(`localStorage.setItem('viti-token', ${JSON.stringify(apiKey)}); location.reload(); true`);
    await waitFor("authenticated web application", () => evaluate(
      `document.readyState === 'complete' && typeof state !== 'undefined' && performance.timeOrigin !== ${previousTimeOrigin}`,
    ));
  }
  registered = await waitFor("WebRTC registration", () => evaluate("state.voiceRegistered === true"));
  initialFinalizedCalls = await evaluate(`${callsFetch}.then(value => value.calls.filter(call =>
    ${incomingCallFilter}).length)`);
  mark("web-ready");

  ringing = await waitFor("incoming web call", () => evaluate("state.incomingCall === true"), 45000);
  mark("web-ringing");
  if (winner === "web") {
    await waitFor("answer instruction", () => existsSync(`${barrierPath}.answer-web`), 45000);
    mark("answer-instruction-seen");
    await evaluate("document.querySelector('#answer-call').click(); true");
    mark("answer-click-returned");
    try {
      active = await waitFor("answered web call", () => evaluate(
        "state.localCall === true && document.querySelector('#call-status').textContent.includes('Call in progress')",
      ), 20000);
    } catch (error) {
      const snapshot = await evaluate(`JSON.stringify({
        local_call: state.localCall === true,
        incoming_call: state.incomingCall === true,
        registered: state.voiceRegistered === true,
        answer_hidden: document.querySelector('#answer-call').hidden === true,
        status_incoming: document.querySelector('#call-status').textContent.includes('Incoming call'),
        status_active: document.querySelector('#call-status').textContent.includes('Call in progress'),
        status_bidirectional: document.querySelector('#call-status').textContent.includes('two-way audio'),
        remote_stream: Boolean(document.querySelector('#remote-audio').srcObject)
      })`);
      writeFileSync(`${barrierPath}.web-diagnostic`, `${snapshot}\n`, { encoding: "utf8", mode: 0o600 });
      throw error;
    }
    bidirectional = await waitFor(
      "bidirectional incoming media",
      () => evaluate("document.querySelector('#call-status').textContent.includes('two-way audio')"),
      30000,
    );
    mark("web-active");
  } else {
    await waitFor("Android answer instruction", () => existsSync(`${barrierPath}.answer-android`), 45000);
    await waitFor("web conference join offer", () => evaluate(
      "state.localCall === false && state.globalCall === true && !document.querySelector('#join-call').hidden",
    ), 30000);
    await evaluate("document.querySelector('#join-call').click(); true");
    joinedConference = await waitFor("web conference media", () => evaluate(
      "state.localCall === true && document.querySelector('#call-status').textContent.includes('two-way audio')",
    ), 30000);
    mark("web-joined");
  }
  await waitFor("release instruction", () => existsSync(`${barrierPath}.release`), 60000);
} finally {
  try {
    await evaluate("document.querySelector('#hangup-call').click(); true");
    await waitFor("call hangup", () => evaluate("state.localCall === false"), 30000);
    historyFinalized = await waitFor(
      "finalized incoming history",
      () => evaluate(`${callsFetch}.then(value => value.calls.filter(call =>
        ${incomingCallFilter}).length > ${initialFinalizedCalls})`),
      30000,
    );
  } catch {
    // The PowerShell wrapper performs its own channel and history checks.
  }
  try {
    await command("Browser.close");
  } catch {
    socket.close();
  }
}

const result = {
  winner,
  registered: Boolean(registered),
  ringing: Boolean(ringing),
  active: Boolean(active),
  bidirectional: Boolean(bidirectional),
  joined_conference: Boolean(joinedConference),
  history_finalized: Boolean(historyFinalized),
};
console.log(JSON.stringify(result));
const winnerSucceeded = winner === "web"
  ? result.active && result.bidirectional
  : result.joined_conference;
if (!(result.registered && result.ringing && winnerSucceeded && result.history_finalized)) process.exitCode = 1;
