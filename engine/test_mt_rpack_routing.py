import ast
import contextlib
import io
from pathlib import Path
import runpy
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock


PATCH = Path(__file__).parent / "patches/asterisk/mt_rpack_routing.py"
TARGET = "/home/asterisk-build/asterisk/res/res_pjsip_messaging.c"


class MTRPACKPatchTest(unittest.TestCase):
    @unittest.skipUnless(shutil.which("gcc"), "C callback behavior requires gcc")
    def test_callback_distinguishes_remote_acceptance_from_local_failures(self):
        tree = ast.parse(PATCH.read_text(encoding="utf-8"))
        replacement = next(ast.literal_eval(node.value) for node in tree.body
                           if isinstance(node, ast.Assign)
                           and any(isinstance(target, ast.Name) and target.id == "FIXED_FN" for target in node.targets))
        callback = replacement[:replacement.index("static pj_status_t send_rpack(")]
        harness = r'''
#include <stdio.h>
#include <string.h>
#include <stdarg.h>
enum { LOG_NOTICE, PJSIP_EVENT_TSX_STATE, PJSIP_EVENT_RX_MSG,
       PJSIP_EVENT_TIMER, PJSIP_EVENT_TRANSPORT_ERROR };
typedef struct { int status_code; } pjsip_transaction;
typedef struct {
    int type;
    struct { struct { pjsip_transaction *tsx; int type; } tsx_state; } body;
} pjsip_event;
static char output[512];
static void ast_log(int level, const char *format, ...) {
    va_list args;
    (void)level;
    va_start(args, format);
    vsnprintf(output, sizeof(output), format, args);
    va_end(args);
}
'''
        cases = r'''
int main(void) {
    pjsip_transaction transaction = {202};
    pjsip_event event = {PJSIP_EVENT_TSX_STATE, {{&transaction, PJSIP_EVENT_RX_MSG}}};
    viti_rpack_result((void *)(uintptr_t)256, &event);
    if (!strstr(output, "rp_ref=255 result=accepted sip_status=202 source=response")) return 1;
    transaction.status_code = 403;
    viti_rpack_result((void *)(uintptr_t)1, &event);
    if (!strstr(output, "rp_ref=0 result=rejected sip_status=403 source=response")) return 2;
    transaction.status_code = 408;
    event.body.tsx_state.type = PJSIP_EVENT_TIMER;
    viti_rpack_result((void *)(uintptr_t)1, &event);
    if (!strstr(output, "result=unconfirmed sip_status=408 source=timeout")) return 3;
    transaction.status_code = 503;
    event.body.tsx_state.type = PJSIP_EVENT_TRANSPORT_ERROR;
    viti_rpack_result((void *)(uintptr_t)1, &event);
    if (!strstr(output, "result=unconfirmed sip_status=503 source=transport_error")) return 4;
    viti_rpack_result((void *)(uintptr_t)1, NULL);
    if (!strstr(output, "result=unconfirmed sip_status=0 source=unknown")) return 5;
    return 0;
}
'''
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / "callback.c"
            binary = Path(directory) / "callback"
            source.write_text(harness + callback + cases, encoding="utf-8")
            subprocess.run(["gcc", "-std=c11", "-Wall", "-Wextra", "-Werror", str(source), "-o", str(binary)], check=True, capture_output=True)
            subprocess.run([str(binary)], check=True, capture_output=True)

    def apply_patch(self, source):
        real_open = open
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "messaging.c"
            target.write_text(source, encoding="utf-8")

            def redirect(path, *args, **kwargs):
                return real_open(target if str(path) == TARGET else path, *args, **kwargs)

            with mock.patch("builtins.open", side_effect=redirect), contextlib.redirect_stdout(io.StringIO()):
                try:
                    runpy.run_path(str(PATCH))
                except SystemExit as error:
                    self.assertEqual(error.code, 0)
            return target.read_text(encoding="utf-8")

    def test_installs_result_callback_without_changing_ack_payload(self):
        before = "static pj_status_t send_rpack(pjsip_rx_data *rdata, unsigned char ack_ref)\n{\nreturn 0;\n}\n"
        after = self.apply_patch(before)
        self.assertIn("PATCH_RPACK_ROUTING4", after)
        self.assertIn("(void *)(uintptr_t)(ack_ref + 1), viti_rpack_result", after)
        self.assertIn("buf[0] = 2", after)
        self.assertIn("buf[1] = ack_ref", after)
        self.assertIn("ast_sip_add_binary_body(tdata, &body, 6)", after)
        self.assertIn("case PJSIP_EVENT_RX_MSG:", after)
        self.assertIn('result = "accepted"', after)
        self.assertIn('source = "timeout"', after)
        self.assertIn('source = "transport_error"', after)
        self.assertIn("result=local_failure", after)
        self.assertEqual(after, self.apply_patch(after))

    def test_upgrades_existing_routing_patch_without_duplicating_functions(self):
        before = "prefix\nstatic pj_status_t send_rpack(pjsip_rx_data *rdata, unsigned char ack_ref)\n{\n/* PATCH_RPACK_ROUTING3 */\nreturn 0;\n}\nsuffix\n"
        after = self.apply_patch(before)
        self.assertEqual(after.count("static void viti_rpack_result("), 1)
        self.assertEqual(after.count("static pj_status_t send_rpack("), 1)
        self.assertTrue(after.startswith("prefix\n"))
        self.assertTrue(after.endswith("\nsuffix\n"))


if __name__ == "__main__":
    unittest.main()
