#!/usr/bin/env python3
"""Request and expose 3GPP SMS delivery evidence from sysmocom Asterisk.

The upstream VoLTE SMS implementation discards RP-ACK/RP-ERROR, parses a
SMS-STATUS-REPORT as an incoming SMS-DELIVER, and does not set TP-SRR.  This
patch keeps a bounded correlation table, requests status reports, and emits a
VitiSMS AMI event for the manager's persistent state machine.
"""

from pathlib import Path


source = Path("res/res_pjsip_messaging.c")
text = source.read_text(encoding="utf-8")
marker = "VITI_SMS_DELIVERY_REPORTS"
if marker in text:
    print("SMS delivery-report patch already applied")
    raise SystemExit(0)


def replace_once(needle, replacement):
    global text
    if text.count(needle) != 1:
        raise SystemExit("expected exactly one SMS patch anchor: %r" % needle[:100])
    text = text.replace(needle, replacement, 1)


replace_once(
    '#include "asterisk/message.h"\n',
    '#include "asterisk/message.h"\n#include "asterisk/manager.h"\n#include <time.h>\n',
)

tracking = r'''
/* VITI_SMS_DELIVERY_REPORTS
 * Keep only protocol correlation metadata; message text never enters this table
 * or the AMI event. References are one octet on the wire, so outstanding values
 * are allocated without reuse whenever possible. Entries expire after 24 hours;
 * a later status report is still emitted with TP-MR + recipient for persistent
 * manager-side correlation. */
#define VITI_SMS_TRACK_MAX 256
#define VITI_SMS_TRACK_TTL 86400
#define VITI_SMS_TOMBSTONE_TTL 900
#define VITI_SMS_SCTS_TOLERANCE 900

struct viti_sms_track {
	int active;
	int terminal;
	unsigned char rp_ref;
	unsigned char tp_ref;
	unsigned long generation;
	time_t created;
	time_t finished;
	char message_id[96];
	char destination[64];
	int part;
	int parts;
};

AST_MUTEX_DEFINE_STATIC(viti_sms_lock);
static struct viti_sms_track viti_sms_tracks[VITI_SMS_TRACK_MAX];
static unsigned char viti_sms_next_rp = 1;
static unsigned char viti_sms_next_tp = 0;
static unsigned long viti_sms_generation;

static void viti_sms_prune_locked(time_t now)
{
	int i;
	for (i = 0; i < VITI_SMS_TRACK_MAX; ++i) {
		if (viti_sms_tracks[i].active &&
			((viti_sms_tracks[i].terminal && now - viti_sms_tracks[i].finished > VITI_SMS_TOMBSTONE_TTL) ||
			 (!viti_sms_tracks[i].terminal && now - viti_sms_tracks[i].created > VITI_SMS_TRACK_TTL))) {
			viti_sms_tracks[i].active = 0;
		}
	}
}

static int viti_sms_reference_in_use_locked(unsigned char rp_ref, unsigned char tp_ref)
{
	int i;
	for (i = 0; i < VITI_SMS_TRACK_MAX; ++i) {
		if (viti_sms_tracks[i].active &&
			(viti_sms_tracks[i].rp_ref == rp_ref || viti_sms_tracks[i].tp_ref == tp_ref)) {
			return 1;
		}
	}
	return 0;
}

static int viti_sms_next_references(unsigned char *rp_ref, unsigned char *tp_ref)
{
	int attempts;
	time_t now = time(NULL);
	ast_mutex_lock(&viti_sms_lock);
	viti_sms_prune_locked(now);
	for (attempts = 0; attempts < 256; ++attempts) {
		unsigned char candidate_rp = viti_sms_next_rp++;
		unsigned char candidate_tp = viti_sms_next_tp++;
		if (!viti_sms_reference_in_use_locked(candidate_rp, candidate_tp)) {
			*rp_ref = candidate_rp;
			*tp_ref = candidate_tp;
			ast_mutex_unlock(&viti_sms_lock);
			return 0;
		}
	}
	ast_mutex_unlock(&viti_sms_lock);
	return -1;
}

static struct viti_sms_track viti_sms_track_store(const char *message_id,
	const char *destination, int part, int parts, unsigned char rp_ref, unsigned char tp_ref)
{
	int i, slot = -1;
	unsigned long oldest = 0;
	struct viti_sms_track result;
	time_t now = time(NULL);
	memset(&result, 0, sizeof(result));
	ast_mutex_lock(&viti_sms_lock);
	viti_sms_prune_locked(now);
	for (i = 0; i < VITI_SMS_TRACK_MAX; ++i) {
		if (!viti_sms_tracks[i].active) {
			slot = i;
			break;
		}
		if (slot < 0 || viti_sms_tracks[i].generation < oldest) {
			slot = i;
			oldest = viti_sms_tracks[i].generation;
		}
	}
	memset(&viti_sms_tracks[slot], 0, sizeof(viti_sms_tracks[slot]));
	viti_sms_tracks[slot].active = 1;
	viti_sms_tracks[slot].rp_ref = rp_ref;
	viti_sms_tracks[slot].tp_ref = tp_ref;
	viti_sms_tracks[slot].generation = ++viti_sms_generation;
	viti_sms_tracks[slot].created = now;
	viti_sms_tracks[slot].part = part;
	viti_sms_tracks[slot].parts = parts;
	ast_copy_string(viti_sms_tracks[slot].message_id, S_OR(message_id, ""), sizeof(viti_sms_tracks[slot].message_id));
	ast_copy_string(viti_sms_tracks[slot].destination, S_OR(destination, ""), sizeof(viti_sms_tracks[slot].destination));
	result = viti_sms_tracks[slot];
	ast_mutex_unlock(&viti_sms_lock);
	return result;
}

static int viti_sms_track_find_rp(unsigned char rp_ref, struct viti_sms_track *result)
{
	int i, found = 0;
	unsigned long newest = 0;
	ast_mutex_lock(&viti_sms_lock);
	viti_sms_prune_locked(time(NULL));
	for (i = 0; i < VITI_SMS_TRACK_MAX; ++i) {
		if (viti_sms_tracks[i].active && viti_sms_tracks[i].rp_ref == rp_ref &&
			(!found || viti_sms_tracks[i].generation > newest)) {
			*result = viti_sms_tracks[i];
			newest = viti_sms_tracks[i].generation;
			found = 1;
		}
	}
	ast_mutex_unlock(&viti_sms_lock);
	return found;
}

static int viti_sms_track_find_tp(unsigned char tp_ref, const char *destination,
	time_t service_centre_time, struct viti_sms_track *result)
{
	int i, found = 0;
	ast_mutex_lock(&viti_sms_lock);
	viti_sms_prune_locked(time(NULL));
	for (i = 0; i < VITI_SMS_TRACK_MAX; ++i) {
		if (!viti_sms_tracks[i].active || viti_sms_tracks[i].tp_ref != tp_ref) {
			continue;
		}
		if (!ast_strlen_zero(destination) && strcmp(viti_sms_tracks[i].destination, destination)) {
			continue;
		}
		if (service_centre_time != (time_t)-1) {
			time_t delta = viti_sms_tracks[i].created - service_centre_time;
			if (delta < 0) {
				delta = -delta;
			}
			if (delta > VITI_SMS_SCTS_TOLERANCE) {
				continue;
			}
		}
		if (found) {
			ast_mutex_unlock(&viti_sms_lock);
			return -1; /* Collision: never guess which message owns the report. */
		}
		*result = viti_sms_tracks[i];
		found = 1;
	}
	ast_mutex_unlock(&viti_sms_lock);
	return found;
}

static void viti_sms_track_finish(unsigned char rp_ref, unsigned char tp_ref)
{
	int i;
	ast_mutex_lock(&viti_sms_lock);
	for (i = 0; i < VITI_SMS_TRACK_MAX; ++i) {
		if (viti_sms_tracks[i].active && viti_sms_tracks[i].rp_ref == rp_ref &&
			viti_sms_tracks[i].tp_ref == tp_ref) {
			viti_sms_tracks[i].terminal = 1;
			viti_sms_tracks[i].finished = time(NULL);
		}
	}
	ast_mutex_unlock(&viti_sms_lock);
}

static void viti_sms_emit(const char *phase, const struct viti_sms_track *track,
	const char *destination, int rp_ref, int tp_ref, int status, int cause,
	time_t service_centre_time)
{
	manager_event(EVENT_FLAG_SYSTEM, "VitiSMS",
		"Phase: %s\r\nMessageID: %s\r\nDestination: %s\r\n"
		"Part: %d\r\nParts: %d\r\nRPReference: %d\r\nTPReference: %d\r\n"
		"Status: %d\r\nCause: %d\r\nServiceCentreTime: %ld\r\n",
		phase, track ? track->message_id : "",
		track && !ast_strlen_zero(track->destination) ? track->destination : S_OR(destination, ""),
		track ? track->part : 1, track ? track->parts : 1,
		track ? track->rp_ref : rp_ref, track ? track->tp_ref : tp_ref, status, cause,
		(long)service_centre_time);
}

static int viti_sms_swapped_bcd(unsigned char value)
{
	int tens = value & 0x0f;
	int units = (value >> 4) & 0x0f;
	if (tens > 9 || units > 9) {
		return -1;
	}
	return tens * 10 + units;
}

static time_t viti_sms_decode_scts(const unsigned char *value)
{
	struct tm decoded;
	int year = viti_sms_swapped_bcd(value[0]);
	int month = viti_sms_swapped_bcd(value[1]);
	int day = viti_sms_swapped_bcd(value[2]);
	int hour = viti_sms_swapped_bcd(value[3]);
	int minute = viti_sms_swapped_bcd(value[4]);
	int second = viti_sms_swapped_bcd(value[5]);
	int zone_tens = value[6] & 0x07;
	int zone_units = (value[6] >> 4) & 0x0f;
	int zone_minutes;
	time_t local;
	if (year < 0 || month < 1 || month > 12 || day < 1 || day > 31 ||
		hour < 0 || hour > 23 || minute < 0 || minute > 59 ||
		second < 0 || second > 60 || zone_units > 9) {
		return (time_t)-1;
	}
	memset(&decoded, 0, sizeof(decoded));
	decoded.tm_year = year >= 70 ? year : year + 100;
	decoded.tm_mon = month - 1;
	decoded.tm_mday = day;
	decoded.tm_hour = hour;
	decoded.tm_min = minute;
	decoded.tm_sec = second;
	local = timegm(&decoded);
	if (local == (time_t)-1) {
		return (time_t)-1;
	}
	zone_minutes = (zone_tens * 10 + zone_units) * 15;
	if (value[6] & 0x08) {
		zone_minutes = -zone_minutes;
	}
	return local - zone_minutes * 60;
}
'''

replace_once(
    "static struct ast_taskprocessor *message_serializer;\n",
    "static struct ast_taskprocessor *message_serializer;\n" + tracking,
)

replacement_parse = r'''static void parse_rpdata(pjsip_rx_data *rdata, struct ast_msg *msg, int *ack_ref)
{
	unsigned char buf[300];
	int len;
	if (!rdata->msg_info.msg->body || !rdata->msg_info.msg->body->len) {
		ast_log(LOG_DEBUG, "No data\n");
		return;
	}
	len = rdata->msg_info.msg->body->print_body(
		rdata->msg_info.msg->body, (char *)buf, sizeof(buf));
	if (len < 2) {
		ast_log(LOG_DEBUG, "MESSAGE RP-DATA is too short or error: %d.\n", len);
		return;
	}

	switch (buf[0]) {
	case 0x01: {
		unsigned char *p;
		unsigned char *tpdu;
		int tpdu_len;
		int sender_len;
		if (len < 3) {
			return;
		}
		sender_len = buf[2] & 0xff;
		if (3 + sender_len + 2 > len) {
			return;
		}
		*ack_ref = buf[1] & 0xff;
		p = &buf[3 + sender_len];
		if ((p[0] & 0xff) != 0) {
			return;
		}
		tpdu_len = p[1] & 0xff;
		if (tpdu_len > len - (p - buf) - 2) {
			tpdu_len = len - (p - buf) - 2;
		}
		tpdu = p + 2;
		if (tpdu_len >= 2 && (tpdu[0] & 3) == 2) {
			char recipient[64] = "";
			int address_len;
			int offset = 2;
			int status;
			time_t service_centre_time;
			unsigned char tp_ref = tpdu[1];
			struct viti_sms_track track;
			memset(&track, 0, sizeof(track));
			if (tpdu_len < 3) {
				return;
			}
			address_len = (tpdu[2] + 5) / 2;
			if (offset + address_len + 15 > tpdu_len) {
				return;
			}
			offset += unpackaddress(recipient, tpdu + offset, sizeof(recipient));
			service_centre_time = viti_sms_decode_scts(tpdu + offset);
			/* Some networks encode TP-SCTS with a stale standard-time offset
			 * during daylight saving time.  A service-centre acceptance time
			 * cannot be materially later than receipt of its status report.
			 * Discard that unreliable correlation hint instead of allowing it
			 * to detach the report from the in-memory submission track. */
			if (service_centre_time != (time_t)-1 &&
				service_centre_time > time(NULL) + VITI_SMS_SCTS_TOLERANCE) {
				service_centre_time = (time_t)-1;
			}
			offset += 14; /* TP-SCTS + TP-DT */
			status = tpdu[offset] & 0xff;
			ast_msg_set_var(msg, "VITI_SMS_PROTOCOL_ONLY", "1");
			if (viti_sms_track_find_tp(tp_ref, recipient, service_centre_time, &track) == 1) {
				viti_sms_emit("status_report", &track, recipient, -1, tp_ref, status, -1, service_centre_time);
				if (status < 0x20 || status >= 0x40) {
					viti_sms_track_finish(track.rp_ref, track.tp_ref);
				}
			} else {
				viti_sms_emit("status_report", NULL, recipient, -1, tp_ref, status, -1, service_centre_time);
			}
			return;
		}
		parse_tpdu(msg, tpdu, tpdu_len);
		return;
	}
	case 0x03: { /* RP-ACK: carrier/SMSC acceptance, not destination delivery. */
		struct viti_sms_track track;
		unsigned char rp_ref = buf[1];
		memset(&track, 0, sizeof(track));
		ast_msg_set_var(msg, "VITI_SMS_PROTOCOL_ONLY", "1");
		if (viti_sms_track_find_rp(rp_ref, &track)) {
			viti_sms_emit("rp_ack", &track, NULL, rp_ref, track.tp_ref, -1, -1, (time_t)-1);
		} else {
			viti_sms_emit("rp_ack", NULL, NULL, rp_ref, -1, -1, -1, (time_t)-1);
		}
		return;
	}
	case 0x05: { /* RP-ERROR: type, reference, cause length, cause value. */
		struct viti_sms_track track;
		unsigned char rp_ref = buf[1];
		int cause = len >= 4 && buf[2] > 0 ? (buf[3] & 0x7f) : -1;
		memset(&track, 0, sizeof(track));
		ast_msg_set_var(msg, "VITI_SMS_PROTOCOL_ONLY", "1");
		if (viti_sms_track_find_rp(rp_ref, &track)) {
			viti_sms_emit("rp_error", &track, NULL, rp_ref, track.tp_ref, -1, cause, (time_t)-1);
			viti_sms_track_finish(track.rp_ref, track.tp_ref);
		} else {
			viti_sms_emit("rp_error", NULL, NULL, rp_ref, -1, -1, cause, (time_t)-1);
		}
		return;
	}
	default:
		ast_log(LOG_WARNING, "Unknown RP-DATA 0x%02x. Dropping message\n", buf[0]);
		ast_msg_set_var(msg, "VITI_SMS_PROTOCOL_ONLY", "1");
		return;
	}
}'''

signature = "static void parse_rpdata(pjsip_rx_data *rdata, struct ast_msg *msg, int *ack_ref)"
start = text.find(signature)
if start < 0:
    raise SystemExit("parse_rpdata function not found")
brace = text.find("{", start)
depth = 0
end = -1
for index in range(brace, len(text)):
    if text[index] == "{":
        depth += 1
    elif text[index] == "}":
        depth -= 1
        if depth == 0:
            end = index + 1
            break
if end < 0:
    raise SystemExit("could not match parse_rpdata braces")
text = text[:start] + replacement_parse + text[end:]

replace_once(
    "\tstatic uint8_t ref = 1;  // TODO: start at random\n\tstatic uint8_t mr = 0;\n",
    "\tconst char *viti_message_id = ast_msg_get_var(mdata->msg, \"VITI_MESSAGE_ID\");\n",
)
replace_once(
    "\t\tbuf[1] = ref++; // Reference\n",
    "\t\tunsigned char rp_ref, tp_ref;\n"
    "\t\tstruct viti_sms_track viti_track;\n"
    "\t\tif (viti_sms_next_references(&rp_ref, &tp_ref)) {\n"
    "\t\t\tstruct viti_sms_track exhausted;\n"
    "\t\t\tmemset(&exhausted, 0, sizeof(exhausted));\n"
    "\t\t\tast_copy_string(exhausted.message_id, S_OR(viti_message_id, \"\"), sizeof(exhausted.message_id));\n"
    "\t\t\tast_copy_string(exhausted.destination, phone, sizeof(exhausted.destination));\n"
    "\t\t\texhausted.part = part + 1;\n"
    "\t\t\texhausted.parts = parts;\n"
    "\t\t\tviti_sms_emit(\"local_failure\", &exhausted, phone, -1, -1, -1, -1, (time_t)-1);\n"
    "\t\t\tast_log(LOG_ERROR, \"No free SMS RP/TP references; refusing ambiguous transmission\\n\");\n"
    "\t\t\treturn -1;\n"
    "\t\t}\n"
    "\t\tbuf[1] = rp_ref; // RP-Message Reference\n",
)
replace_once(
    "\t\t*p++ = 0x11 | ((parts > 1) ? 0x40 : 0);                      /* SMS_DATA */\n"
    "\t\t*p++ = mr++;  /* TP-MR */\n",
    "\t\t/* TP-SRR requests SMS-STATUS-REPORT; it does not request a read receipt. */\n"
    "\t\t*p++ = 0x31 | ((parts > 1) ? 0x40 : 0);\n"
    "\t\t*p++ = tp_ref;  /* TP-MR */\n",
)
replace_once(
    "\t\t/* TODO: resend if no RP-ACK is received */\n"
    "\t\tint status = volte_send_rp_data(mdata, orig_uri, endpoint, buf, p - buf);\n"
    "\t\tif (status < 0)\n"
    "\t\t\toverall_status = status;\n",
    "\t\t/* Store before transmission so a fast RP-ACK cannot beat correlation. */\n"
    "\t\tviti_track = viti_sms_track_store(viti_message_id, phone, part + 1, parts, rp_ref, tp_ref);\n"
    "\t\tint status = volte_send_rp_data(mdata, orig_uri, endpoint, buf, p - buf);\n"
    "\t\tif (status < 0) {\n"
    "\t\t\toverall_status = status;\n"
    "\t\t\tviti_sms_emit(\"local_failure\", &viti_track, phone, rp_ref, tp_ref, -1, -1, (time_t)-1);\n"
    "\t\t\tviti_sms_track_finish(rp_ref, tp_ref);\n"
    "\t\t} else {\n"
    "\t\t\tviti_sms_emit(\"submitted\", &viti_track, phone, rp_ref, tp_ref, -1, -1, (time_t)-1);\n"
    "\t\t}\n",
)

source.write_text(text, encoding="utf-8")
print("patched VoLTE SMS delivery reports and AMI protocol events")
