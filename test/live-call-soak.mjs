import { execFileSync } from 'node:child_process'
import { appendFileSync, existsSync, mkdirSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import process from 'node:process'
import { fileURLToPath } from 'node:url'

import { LiveCallSoakTracker } from './live-call-soak-core.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const composeRoot = path.resolve(process.env.VITI_COMPOSE_ROOT || '')
const outputPath = path.resolve(process.env.VITI_SOAK_OUTPUT || '')
const numberSetting = (name, fallback, minimum = 0) => {
  const value = Number(process.env[name] || fallback)
  if (!Number.isFinite(value) || value < minimum) throw new Error(`${name} must be a number greater than or equal to ${minimum}`)
  return value
}
const sampleSeconds = numberSetting('VITI_SAMPLE_SECONDS', '5', 1)
const maxRunSeconds = numberSetting('VITI_MAX_RUN_SECONDS', String(5 * 60 * 60), 1)
const tracker = new LiveCallSoakTracker({
  minCallSeconds: numberSetting('VITI_MIN_CALL_SECONDS', '7200'),
  minIkeRenewals: numberSetting('VITI_MIN_IKE_RENEWALS', '2'),
  minChildRenewals: numberSetting('VITI_MIN_CHILD_RENEWALS', '2'),
  postCallSeconds: numberSetting('VITI_POST_CALL_SECONDS', '120'),
  maxMediaStallSeconds: numberSetting('VITI_MAX_MEDIA_STALL_SECONDS', '15', 1),
})

if (process.env.VITI_LIVE_SOAK !== '1') throw new Error('set VITI_LIVE_SOAK=1 to opt in to live observation')
if (!process.env.VITI_COMPOSE_ROOT || !existsSync(path.join(composeRoot, 'compose.yml'))) {
  throw new Error('VITI_COMPOSE_ROOT must identify the deployed Compose directory')
}
if (!process.env.VITI_SOAK_OUTPUT || !path.isAbsolute(process.env.VITI_SOAK_OUTPUT)) {
  throw new Error('VITI_SOAK_OUTPUT must be an absolute JSONL path outside the repository')
}
const relativeOutput = path.relative(repoRoot, outputPath)
if (relativeOutput === '' || (!relativeOutput.startsWith(`..${path.sep}`) && relativeOutput !== '..')) {
  throw new Error('VITI_SOAK_OUTPUT must be outside the repository')
}
if (existsSync(outputPath) || existsSync(`${outputPath}.summary.json`)) {
  throw new Error('VITI_SOAK_OUTPUT must identify a new file')
}
mkdirSync(path.dirname(outputPath), { recursive: true, mode: 0o700 })

const snapshotProgram = String.raw`
import hashlib, json, re, subprocess

def run(command):
    try:
        return subprocess.check_output(command, stderr=subprocess.DEVNULL, text=True, timeout=4)
    except Exception:
        return ""

def present(command):
    try:
        return subprocess.run(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=4).returncode == 0
    except Exception:
        return False

registrations = run(["asterisk", "-rx", "pjsip show registrations"])
channels = run(["asterisk", "-rx", "core show channels count"])
channelstats = run(["asterisk", "-rx", "pjsip show channelstats"])
sas = run(["swanctl", "--list-sas", "--raw"])
ike_ids, child_ids = [], []
for event in sas.split("list-sa event ")[1:]:
    ids = re.findall(r"uniqueid=([0-9]+)", event)
    if ids:
        ike_ids.append(ids[0])
        child_ids.extend(ids[1:])
active = re.search(r"(?m)^([0-9]+) active channels?$", channels)
values = lambda name: [int(value) for value in re.findall(name + r"=([0-9]+)", sas)]
media_streams, media_rx_packets, media_tx_packets = [], 0, 0
for line in channelstats.splitlines():
    fields = line.split()
    if len(fields) < 12:
        continue
    try:
        stats = fields[-9:]
        channel_id = fields[-12]
        rx_packets = int(stats[0])
        tx_packets = int(stats[4])
        float(stats[3])
        float(stats[7])
        float(stats[8])
    except (ValueError, IndexError):
        continue
    media_rx_packets += rx_packets
    media_tx_packets += tx_packets
    media_streams.append({
        "id": hashlib.sha256(channel_id.encode()).hexdigest()[:16],
        "rx_packets": rx_packets,
        "tx_packets": tx_packets,
    })
media_streams.sort(key=lambda stream: stream["id"])
print(json.dumps({
    "snapshot_ok": bool(channels and registrations and channelstats),
    "charon": present(["pgrep", "-x", "charon"]),
    "python": present(["pgrep", "-f", "[s]wu_ike.py"]),
    "tunnel": present(["ip", "link", "show", "ipsec0"]),
    "ims": bool(re.search(r"(?m)^\s*volte_ims/.*\sRegistered\s", registrations)),
    "active_channels": int(active.group(1)) if active else 0,
    "ike_ids": ike_ids,
    "child_ids": child_ids,
    "bytes_in": sum(values("bytes-in")),
    "bytes_out": sum(values("bytes-out")),
    "media_channels": len(media_streams),
    "media_rx_packets": media_rx_packets,
    "media_tx_packets": media_tx_packets,
    "media_streams": media_streams,
}, separators=(",", ":")))
`

const delay = milliseconds => new Promise(resolve => setTimeout(resolve, milliseconds))
const deadline = Date.now() + maxRunSeconds * 1000
let stopRequested = false
process.on('SIGINT', () => { stopRequested = true })
process.on('SIGTERM', () => { stopRequested = true })

while (tracker.phase !== 'complete' && Date.now() < deadline && !stopRequested) {
  let sample
  try {
    const raw = execFileSync('docker', [
      'compose', 'exec', '-T', 'engine', 'python3', '-c', snapshotProgram,
    ], { cwd: composeRoot, encoding: 'utf8', timeout: 10_000 })
    sample = { timestamp: new Date().toISOString(), ...JSON.parse(raw) }
  } catch (error) {
    sample = { timestamp: new Date().toISOString(), snapshot_ok: false, error: error.message }
  }
  appendFileSync(outputPath, `${JSON.stringify(sample)}\n`, { encoding: 'utf8', mode: 0o600 })
  tracker.observe(sample)
  if (tracker.phase !== 'complete') await delay(sampleSeconds * 1000)
}

if (stopRequested) tracker.finish('observation interrupted')
else if (tracker.phase !== 'complete') tracker.finish('maximum observation time reached')
const summary = tracker.summary()
const summaryPath = `${outputPath}.summary.json`
writeFileSync(summaryPath, `${JSON.stringify(summary, null, 2)}\n`, { encoding: 'utf8', mode: 0o600 })
console.log(JSON.stringify({ ...summary, output: outputPath, summary: summaryPath }))
if (!summary.accepted) process.exitCode = 1
