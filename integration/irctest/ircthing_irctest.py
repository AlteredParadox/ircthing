"""irctest controller for ircthing (https://github.com/progval/irctest).

Runs the ircd-web binary configured with a single network pointing at
irctest's mock server, so irctest's client_tests can drive our IRC
handshake (CAP negotiation, SASL, TLS, STS). Invoked via `make irctest`,
which puts this directory on PYTHONPATH and passes
--controller=ircthing_irctest.

The binary path comes from the IRCTHING_BIN environment variable
(absolute; set by the Makefile).
"""

import json
import os
from typing import Optional, Type

from irctest import authentication, tls
from irctest.basecontrollers import BaseClientController, DirectoryBasedController

# bcrypt of "irctest" — the web login is never exercised by irctest, but
# the config requires a user.
PASSWORD_HASH = "$2a$10$/vXcvxwnd0BAE188Vf9aSOFQFZeGKQsf1817JpdYiDhibk6nh7QQ."


class IrcthingController(BaseClientController, DirectoryBasedController):
    software_name = "ircthing"
    supported_sasl_mechanisms = {"PLAIN", "SCRAM-SHA-256", "EXTERNAL"}
    supports_sts = True

    def run(
        self,
        hostname: str,
        port: int,
        auth: Optional[authentication.Authentication],
        tls_config: Optional[tls.TlsConfig] = None,
    ) -> None:
        # Runs the client with the config given as arguments. run() may be
        # called again after terminate() (the STS persistence test); the
        # directory — and with it the SQLite database holding STS policies —
        # survives until kill().
        assert self.proc is None
        self.create_config()
        assert self.directory

        network = {
            "name": "testnet",
            "addr": f"{hostname}:{port}",
            "tls": bool(tls_config and tls_config.enable),
            "allow_plaintext": True,
            "nick": "ircthing1",
        }
        if tls_config and tls_config.trusted_fingerprints:
            network["trusted_fingerprints"] = list(tls_config.trusted_fingerprints)
        if auth:
            mechanisms = [mech.to_string() for mech in auth.mechanisms]
            network["sasl"] = {
                "mechanism": mechanisms[0],
                "login": auth.username or "",
                "password": auth.password or "",
            }

        config = {
            "listen": "127.0.0.1:0",
            "database": str(self.directory / "ircthing.db"),
            "user": {"username": "irctest", "password_hash": PASSWORD_HASH},
            "networks": [network],
        }
        with self.open_file("config.json", mode="w") as fd:
            json.dump(config, fd)

        self.proc = self.execute(
            [
                os.environ["IRCTHING_BIN"],
                "-config",
                self.directory / "config.json",
            ]
        )


def get_irctest_controller_class() -> Type[IrcthingController]:
    return IrcthingController
