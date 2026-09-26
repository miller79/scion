#!/usr/bin/env python3
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""Unit tests for the Copilot harness provisioner.

Run with:  python3 -m unittest provision_test -v
"""

from __future__ import annotations

import importlib.util
import os
import tempfile
import unittest
from contextlib import contextmanager

PROVISION_PATH = os.path.join(os.path.dirname(__file__), "provision.py")
SPEC = importlib.util.spec_from_file_location("copilot_provision", PROVISION_PATH)
assert SPEC is not None
provision = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(provision)

scion_harness = provision.scion_harness


@contextmanager
def temporary_home(path: str):
    old_home = os.environ.get("HOME")
    os.environ["HOME"] = path
    try:
        yield
    finally:
        if old_home is None:
            os.environ.pop("HOME", None)
        else:
            os.environ["HOME"] = old_home


class BaseTelemetryTest(unittest.TestCase):
    """Base class that isolates tests from host SCION_/OTEL_ env vars."""

    _saved_env: dict[str, str]

    def setUp(self) -> None:
        super().setUp()
        self._saved_env = {}
        for key in list(os.environ):
            if key.startswith(("SCION_", "OTEL_")):
                self._saved_env[key] = os.environ.pop(key)

    def tearDown(self) -> None:
        # Remove any SCION_/OTEL_ vars that tests may have set.
        for key in list(os.environ):
            if key.startswith(("SCION_", "OTEL_")):
                os.environ.pop(key, None)
        # Restore original env vars.
        os.environ.update(self._saved_env)
        super().tearDown()


class TelemetryEnabledTest(unittest.TestCase):
    """Tests for the _telemetry_enabled helper."""

    def test_none_returns_false(self) -> None:
        self.assertFalse(provision._telemetry_enabled(None))

    def test_empty_dict_returns_false(self) -> None:
        self.assertFalse(provision._telemetry_enabled({}))

    def test_enabled_true(self) -> None:
        self.assertTrue(provision._telemetry_enabled({"enabled": True}))

    def test_enabled_none_defaults_true(self) -> None:
        self.assertTrue(provision._telemetry_enabled({"enabled": None}))

    def test_enabled_false(self) -> None:
        self.assertFalse(provision._telemetry_enabled({"enabled": False}))


class BuildTelemetryEnvTest(BaseTelemetryTest):
    """Tests for _build_telemetry_env."""

    def test_defaults_enable_copilot_otel_to_local_http_receiver(self) -> None:
        env = provision._build_telemetry_env({"enabled": True}, None)
        self.assertEqual(env["COPILOT_OTEL_ENABLED"], "true")
        self.assertEqual(env["COPILOT_OTEL_EXPORTER_TYPE"], "otlp-http")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:4318")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_PROTOCOL"], "http/protobuf")
        self.assertEqual(env["OTEL_METRICS_EXPORTER"], "otlp")
        self.assertEqual(env["OTEL_LOGS_EXPORTER"], "otlp")
        self.assertEqual(env["OTEL_METRIC_EXPORT_INTERVAL"], "30000")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE"], "cumulative")
        # The flag Copilot CLI never read.
        self.assertNotIn("COPILOT_TELEMETRY_ENABLED", env)

    def test_cloud_backend_settings_do_not_redirect_copilot(self) -> None:
        # telemetry.cloud and SCION_OTEL_* describe sciontool's hop to the
        # cloud; Copilot must still export to the local receiver.
        telemetry = {
            "enabled": True,
            "cloud": {
                "endpoint": "https://otel.example.com:4317",
                "protocol": "grpc",
                "headers": {"authorization": "Bearer tok"},
                "tls": {"ca_file": "/etc/scion/ca.pem"},
            },
        }
        env_overlay = {
            "SCION_OTEL_ENDPOINT": "http://scion-collector:4317",
            "SCION_OTEL_PROTOCOL": "grpc",
            "SCION_OTEL_HEADERS": '{"authorization": "Bearer other"}',
        }
        env = provision._build_telemetry_env(telemetry, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:4318")
        self.assertEqual(env["COPILOT_OTEL_EXPORTER_TYPE"], "otlp-http")
        self.assertNotIn("OTEL_EXPORTER_OTLP_HEADERS", env)
        self.assertNotIn("OTEL_EXPORTER_OTLP_CERTIFICATE", env)

    def test_local_http_port_override(self) -> None:
        env = provision._build_telemetry_env({"enabled": True}, {"SCION_OTEL_HTTP_PORT": "14318"})
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:14318")

    def test_copilot_grpc_override_uses_local_grpc_port(self) -> None:
        env_overlay = {"SCION_COPILOT_OTEL_PROTOCOL": "grpc", "SCION_OTEL_GRPC_PORT": "14317"}
        env = provision._build_telemetry_env({"enabled": True}, env_overlay)
        self.assertEqual(env["COPILOT_OTEL_EXPORTER_TYPE"], "otlp-grpc")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_PROTOCOL"], "grpc")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:14317")

    def test_invalid_local_port_rejected(self) -> None:
        with self.assertRaises(scion_harness.ProvisionError):
            provision._build_telemetry_env({"enabled": True}, {"SCION_OTEL_HTTP_PORT": "not-a-port"})

    def test_explicit_copilot_collector_with_headers_and_ca(self) -> None:
        env_overlay = {
            "SCION_COPILOT_OTEL_ENDPOINT": "https://collector.example:4318",
            "SCION_COPILOT_OTEL_HEADERS": '{"authorization": "Bearer tok", "x-meta": "val"}',
            "SCION_COPILOT_OTEL_CA_FILE": "/etc/scion/ca.pem",
        }
        env = provision._build_telemetry_env({"enabled": True}, env_overlay)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "https://collector.example:4318")
        # Values must be percent-encoded per the OTel SDK spec.
        self.assertEqual(env["OTEL_EXPORTER_OTLP_HEADERS"], "authorization=Bearer%20tok,x-meta=val")
        self.assertEqual(env["OTEL_EXPORTER_OTLP_CERTIFICATE"], "/etc/scion/ca.pem")

    def test_copilot_headers_ignored_without_explicit_endpoint(self) -> None:
        env_overlay = {"SCION_COPILOT_OTEL_HEADERS": '{"authorization": "Bearer tok"}'}
        env = provision._build_telemetry_env({"enabled": True}, env_overlay)
        self.assertNotIn("OTEL_EXPORTER_OTLP_HEADERS", env)

    def test_os_environ_fallback(self) -> None:
        os.environ["SCION_OTEL_HTTP_PORT"] = "24318"
        env = provision._build_telemetry_env({"enabled": True}, None)
        self.assertEqual(env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:24318")


class ResolveProtocolTest(BaseTelemetryTest):
    """Tests for _resolve_protocol."""

    def test_default_is_http(self) -> None:
        self.assertEqual(provision._resolve_protocol(None, None), "http")

    def test_cloud_protocol_ignored(self) -> None:
        self.assertEqual(provision._resolve_protocol({"cloud": {"protocol": "grpc"}}, {"SCION_OTEL_PROTOCOL": "grpc"}), "http")

    def test_copilot_override(self) -> None:
        self.assertEqual(provision._resolve_protocol(None, {"SCION_COPILOT_OTEL_PROTOCOL": "grpc"}), "grpc")
        self.assertEqual(provision._resolve_protocol(None, {"SCION_COPILOT_OTEL_PROTOCOL": "otlp-grpc"}), "grpc")


class ResolveEndpointOsEnvTest(BaseTelemetryTest):
    """os.environ handling in _resolve_endpoint."""

    def test_generic_scion_endpoint_ignored(self) -> None:
        os.environ["SCION_OTEL_ENDPOINT"] = "http://from-os-env:4317"
        self.assertEqual(provision._resolve_endpoint(None, {}), "http://127.0.0.1:4318")

    def test_copilot_os_environ_honoured(self) -> None:
        os.environ["SCION_COPILOT_OTEL_ENDPOINT"] = "http://copilot-os:4318"
        os.environ["SCION_OTEL_ENDPOINT"] = "http://generic-os:4317"
        self.assertEqual(provision._resolve_endpoint(None, {}), "http://copilot-os:4318")

    def test_env_overlay_beats_os_environ(self) -> None:
        os.environ["SCION_COPILOT_OTEL_ENDPOINT"] = "http://from-os-env:4318"
        env = {"SCION_COPILOT_OTEL_ENDPOINT": "http://from-overlay:4318"}
        self.assertEqual(provision._resolve_endpoint(None, env), "http://from-overlay:4318")


class ResolveProtocolOsEnvTest(BaseTelemetryTest):
    """os.environ handling in _resolve_protocol."""

    def test_generic_scion_protocol_ignored(self) -> None:
        os.environ["SCION_OTEL_PROTOCOL"] = "grpc"
        self.assertEqual(provision._resolve_protocol(None, {}), "http")

    def test_copilot_os_environ_honoured(self) -> None:
        os.environ["SCION_COPILOT_OTEL_PROTOCOL"] = "grpc"
        self.assertEqual(provision._resolve_protocol(None, {}), "grpc")

    def test_env_overlay_beats_os_environ(self) -> None:
        os.environ["SCION_COPILOT_OTEL_PROTOCOL"] = "http"
        env = {"SCION_COPILOT_OTEL_PROTOCOL": "grpc"}
        self.assertEqual(provision._resolve_protocol(None, env), "grpc")


class HeadersEnvTest(BaseTelemetryTest):
    """Header handling in _build_telemetry_env."""

    def test_generic_scion_headers_never_copied(self) -> None:
        import json as _json

        os.environ["SCION_OTEL_HEADERS"] = _json.dumps({"authorization": "Bearer tok"})
        env = provision._build_telemetry_env({"enabled": True}, {"SCION_OTEL_HEADERS": _json.dumps({"x": "1"})})
        self.assertNotIn("OTEL_EXPORTER_OTLP_HEADERS", env)

    def test_copilot_headers_from_os_environ_with_explicit_endpoint(self) -> None:
        import json as _json

        os.environ["SCION_COPILOT_OTEL_ENDPOINT"] = "https://collector.example:4318"
        os.environ["SCION_COPILOT_OTEL_HEADERS"] = _json.dumps({"authorization": "Bearer tok"})
        env = provision._build_telemetry_env({"enabled": True}, {})
        self.assertEqual(env["OTEL_EXPORTER_OTLP_HEADERS"], "authorization=Bearer%20tok")

    def test_invalid_json_yields_no_headers(self) -> None:
        env_overlay = {
            "SCION_COPILOT_OTEL_ENDPOINT": "https://collector.example:4318",
            "SCION_COPILOT_OTEL_HEADERS": "not-json",
        }
        env = provision._build_telemetry_env({"enabled": True}, env_overlay)
        self.assertNotIn("OTEL_EXPORTER_OTLP_HEADERS", env)


class CaFileEnvTest(BaseTelemetryTest):
    """TLS CA handling in _build_telemetry_env."""

    def test_generic_scion_ca_ignored(self) -> None:
        os.environ["SCION_OTEL_CA_FILE"] = "/os-env/ca.pem"
        env = provision._build_telemetry_env({"enabled": True}, {"SCION_OTEL_CA_FILE": "/x.pem"})
        self.assertNotIn("OTEL_EXPORTER_OTLP_CERTIFICATE", env)

    def test_copilot_ca_from_os_environ_with_explicit_endpoint(self) -> None:
        os.environ["SCION_COPILOT_OTEL_ENDPOINT"] = "https://collector.example:4318"
        os.environ["SCION_COPILOT_OTEL_CA_FILE"] = "/copilot/ca.pem"
        env = provision._build_telemetry_env({"enabled": True}, {})
        self.assertEqual(env["OTEL_EXPORTER_OTLP_CERTIFICATE"], "/copilot/ca.pem")

    def test_no_ca_file_when_absent(self) -> None:
        env = provision._build_telemetry_env({"enabled": True}, {})
        self.assertNotIn("OTEL_EXPORTER_OTLP_CERTIFICATE", env)


if __name__ == "__main__":
    unittest.main()
