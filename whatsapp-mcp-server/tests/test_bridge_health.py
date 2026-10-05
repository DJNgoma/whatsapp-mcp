from unittest.mock import patch, Mock
import whatsapp


def test_connected_legacy_bridge_is_not_fresh():
    response = Mock()
    response.json.return_value = {"connected": True, "logged_in": True}
    with patch.object(whatsapp.requests, "get", return_value=response), patch.object(whatsapp.os.path, "isfile", return_value=False):
        result = whatsapp.get_bridge_status()
    assert result["reads_may_be_stale"] is True
    assert "unverified" in result["coverage"]


def test_database_error_overrides_freshness_claim():
    response = Mock()
    response.json.return_value = {"connected": True, "logged_in": True, "reads_may_be_stale": False, "database_error": "locked"}
    with patch.object(whatsapp.requests, "get", return_value=response), patch.object(whatsapp.os.path, "isfile", return_value=False):
        result = whatsapp.get_bridge_status()
    assert result["reads_may_be_stale"] is True
