"""Delivery contract: exact recipient receipts, no retry on ambiguity."""
import unittest
from unittest import mock
import delivery

class DeliveryTests(unittest.TestCase):
    def test_accepted_send_waits_for_exact_receipt(self):
        proof={"success":True,"message_id":"id-1","chat_jid":"123@lid","delivery_status":"delivered","delivery_confirmed":True}
        with mock.patch.object(delivery,"_bridge_api_request",return_value=proof) as request:
            result=delivery.finish_send("123@lid",(True,"accepted","id-1"))
        self.assertTrue(result["success"])
        self.assertTrue(result["delivery_confirmed"])
        self.assertEqual(request.call_args.kwargs["params"]["message_id"],"id-1")
        self.assertGreater(request.call_args.kwargs["params"]["wait_seconds"],0)

    def test_pending_and_status_failure_never_claim_delivery(self):
        for response in ({"success":True,"delivery_status":"pending","delivery_confirmed":False},{"success":False,"message":"bridge offline"}):
            with self.subTest(response=response),mock.patch.object(delivery,"_bridge_api_request",return_value=response) as request:
                result=delivery.finish_send("123@lid",(True,"accepted","id-1"))
            self.assertTrue(result["success"])
            self.assertFalse(result["delivery_confirmed"])
            self.assertEqual(result["message_id"],"id-1")
            self.assertEqual(request.call_args.args[0],"GET")
            self.assertIn("Do not resend",result["message"])

    def test_wrong_message_proof_is_rejected(self):
        with mock.patch.object(delivery,"_bridge_api_request",return_value={"success":True,"message_id":"other","chat_jid":"123@lid","delivery_confirmed":True,"delivery_status":"read"}):
            result=delivery.finish_send("123@lid",(True,"accepted","id-1"))
        self.assertFalse(result["delivery_confirmed"])

    def test_ambiguous_send_does_not_poll_or_retry(self):
        with mock.patch.object(delivery,"_bridge_api_request") as request:
            result=delivery.finish_send("123@lid",(False,"Request timed out; outcome unknown",None))
        request.assert_not_called()
        self.assertFalse(result["delivery_confirmed"])
        self.assertEqual(result["send_status"],"unknown")
