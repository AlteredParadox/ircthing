# ircthing — a self-hosted, always-connected web IRC client.
# Copyright (C) 2026 AlteredParadox
#
# This program is free software: you can redistribute it and/or modify it
# under the terms of the GNU Affero General Public License as published by
# the Free Software Foundation, either version 3 of the License, or (at your
# option) any later version.
#
# This program is distributed in the hope that it will be useful, but WITHOUT
# ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
# FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
# for more details.
#
# You should have received a copy of the GNU Affero General Public License
# along with this program. If not, see <https://www.gnu.org/licenses/>.

"""Run irctest's SASL client cases over their existing TLS fixture.

Upstream irctest's generic SASL tests create a plaintext mock listener and do
not offer a TLS parameter to ``negotiateCapabilities``. ircthing deliberately
refuses to send password authentication on plaintext, so those tests would
otherwise exercise only that refusal. This local pytest plugin changes just
the authenticated ``readCapLs`` setup to use irctest's pinned GOOD certificate;
the upstream SASL test bodies and protocol assertions remain untouched.
"""


def pytest_configure(config):
    del config
    # Delay irctest imports until pytest has loaded its conftest and registered
    # assertion rewriting; importing cases at plugin-discovery time produces a
    # noisy PytestAssertRewriteWarning and can hide useful rewritten failures.
    from irctest import cases, tls
    from irctest.client_tests.tls import GOOD_CERT, GOOD_FINGERPRINT, GOOD_KEY

    original_read_cap_ls = cases.BaseClientTestCase.readCapLs

    def secure_sasl_read_cap_ls(self, auth=None, tls_config=None):
        if auth is None or tls_config is not None:
            return original_read_cap_ls(self, auth, tls_config)

        original_accept = self.acceptClient

        def accept_with_tls(*args, **kwargs):
            kwargs.setdefault("tls_cert", GOOD_CERT)
            kwargs.setdefault("tls_key", GOOD_KEY)
            return original_accept(*args, **kwargs)

        self.acceptClient = accept_with_tls
        try:
            return original_read_cap_ls(
                self,
                auth,
                tls.TlsConfig(enable=True, trusted_fingerprints=[GOOD_FINGERPRINT]),
            )
        finally:
            self.acceptClient = original_accept

    cases.BaseClientTestCase.readCapLs = secure_sasl_read_cap_ls
