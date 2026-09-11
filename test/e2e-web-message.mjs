const cdpPort = process.env.VITI_CDP_PORT || "9237";
const pageURL = process.env.VITI_WEB_URL || "http://127.0.0.1:18788/#messages";
const apiKey = process.env.VITI_TEST_API_KEY || "";
const destination = process.env.VITI_TEST_DESTINATION || "";
const marker = process.env.VITI_TEST_MESSAGE_MARKER || "";
const messageText = process.env.VITI_TEST_MESSAGE_TEXT || marker;
const cdpBase = `http://127.0.0.1:${cdpPort}`;

if (!/^Viti-(?:inbound|outbound)-check-[0-9]+$/.test(marker)) {
  throw new Error("VITI_TEST_MESSAGE_MARKER is missing or invalid");
}
if (destination && !/^\+[1-9][0-9]{7,14}$/.test(destination)) {
  throw new Error("VITI_TEST_DESTINATION is not E.164");
}
if (!messageText.startsWith(marker) || [...messageText].length > 1000) {
  throw new Error("VITI_TEST_MESSAGE_TEXT must start with the marker and contain at most 1000 characters");
}

const delay = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

async function waitFor(label, probe, timeoutMilliseconds = 30000) {
  const deadline = Date.now() + timeoutMilliseconds;
  while (Date.now() < deadline) {
    try {
      if (await probe()) return true;
    } catch {
      // Navigation and refresh briefly invalidate the execution context.
    }
    await delay(250);
  }
  throw new Error(`${label} timed out`);
}

await waitFor("Chrome DevTools", async () => {
  try {
    return (await fetch(`${cdpBase}/json/version`)).ok;
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

let submitted = false;
let visible = false;
try {
  await command("Runtime.enable");
  await waitFor(
    "web application",
    () => evaluate("document.readyState === 'complete' && typeof state !== 'undefined'"),
  );
  if (apiKey) {
    const previousTimeOrigin = await evaluate("performance.timeOrigin");
    await evaluate(`localStorage.setItem('viti-token', ${JSON.stringify(apiKey)}); location.reload(); true`);
    await waitFor(
      "authenticated web application",
      () => evaluate(`document.readyState === 'complete' && typeof state !== 'undefined' && performance.timeOrigin !== ${previousTimeOrigin}`),
    );
  }
  await evaluate("setView('messages'); true");
  if (destination) {
    submitted = await evaluate(`(() => {
      const to = document.querySelector('#message-to');
      const text = document.querySelector('#message-text');
      to.value = ${JSON.stringify(destination)};
      text.value = ${JSON.stringify(messageText)};
      text.dispatchEvent(new Event('input', { bubbles: true }));
      const form = document.querySelector('#message-form');
      form.requestSubmit(form.querySelector('[type="submit"]'));
      return true;
    })()`);
  } else {
    await evaluate("refreshLists().then(() => true)");
  }
  visible = await waitFor(
    "message in web history",
    () => evaluate(`document.querySelector('#message-list').textContent.includes(${JSON.stringify(marker)})`),
    120000,
  );
} finally {
  try {
    await command("Browser.close");
  } catch {
    socket.close();
  }
}

const result = {
  submitted: destination ? Boolean(submitted) : null,
  visible: Boolean(visible),
  text_length: [...messageText].length,
};
console.log(JSON.stringify(result));
if (!visible || (destination && !submitted)) process.exitCode = 1;
