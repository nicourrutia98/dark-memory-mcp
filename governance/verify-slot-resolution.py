#!/usr/bin/env python3
"""Verificacion por el CABLE: la resolucion del proyecto por la sesion.

QUE MIDE ESTO. El bug que se arregla solo se ve cuando hay DOS sesiones
en el MISMO proceso MCP, de proyectos DISTINTOS: una sesion mueve el slot
global de proyecto activo, y la escritura de la otra sesion acaba en el
proyecto equivocado. En un proceso con una sola sesion no se reproduce.

Por eso esta verificacion habla JSON-RPC 2.0 por stdio con el binario
real (no con la libreria): el mismo camino que recorre el harness.

EL ESCENARIO, en este orden:
  1. se crean dos proyectos, "alfa" y "beta";
  2. se abre la sesion A en "alfa";
  3. se escribe UNA memoria desde la sesion A -> tiene que caer en "alfa";
  4. se abre la sesion B en "beta" -> ESO mueve el slot global a "beta";
  5. se escribe OTRA memoria desde la sesion A (la sesion A sigue siendo de
     "alfa") -> tiene que caer en "alfa", NO en "beta";
  6. se cierra la sesion A -> tiene que cerrarse (antes fallaba con
     "identity unavailable for this session");
  7. se comprueba el rastro: las filas de write_audit de las escrituras de
     la sesion A tienen que estar en "alfa".

COMO SE USA:
    python3 governance/verify-slot-resolution.py --bin RUTA/DEL/BINARIO

CODIGOS: 0 todo correcto · 1 el contrato NO se cumple (bug presente) ·
         2 error de transporte o de la tool.
"""

import argparse
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile

PROTOCOL_VERSION = "2025-06-18"


class MCP:
    """Cliente MCP minimo sobre stdio: una linea JSON por mensaje.

    El servidor escribe sus logs en stdout junto al JSON-RPC, asi que se
    descarta toda linea que no empiece por '{' y se correlaciona por id.
    """

    def __init__(self, binary, env=None, timeout=90):
        self.proc = subprocess.Popen(
            [binary],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            env=env,
            text=True,
            bufsize=1,
        )
        self._id = 0
        self.timeout = timeout

    def _send(self, obj):
        self.proc.stdin.write(json.dumps(obj, ensure_ascii=False) + "\n")
        self.proc.stdin.flush()

    def _read(self):
        while True:
            line = self.proc.stdout.readline()
            if not line:
                raise RuntimeError("el servidor cerro stdout")
            line = line.strip()
            if not line.startswith("{"):
                continue
            try:
                return json.loads(line)
            except json.JSONDecodeError:
                continue

    def request(self, method, params=None):
        self._id += 1
        rid = self._id
        self._send({"jsonrpc": "2.0", "id": rid, "method": method,
                    "params": params or {}})
        while True:
            msg = self._read()
            if msg.get("id") != rid:
                continue
            if "error" in msg:
                raise RuntimeError(json.dumps(msg["error"], ensure_ascii=False))
            return msg.get("result", {})

    def notify(self, method, params=None):
        self._send({"jsonrpc": "2.0", "method": method, "params": params or {}})

    def initialize(self):
        res = self.request("initialize", {
            "protocolVersion": PROTOCOL_VERSION,
            "capabilities": {},
            "clientInfo": {"name": "verify-slot-resolution.py", "version": "1.0"},
        })
        self.notify("notifications/initialized")
        return res

    def call(self, name, arguments):
        res = self.request("tools/call", {"name": name, "arguments": arguments or {}})
        # El servidor devuelve el sobre {"data": {...}} dentro del texto.
        texto = ""
        for c in res.get("content", []) or []:
            if c.get("type") == "text":
                texto += c.get("text", "")
        if res.get("isError"):
            raise RuntimeError("%s: %s" % (name, texto or res))
        try:
            envoltura = json.loads(texto)
        except (json.JSONDecodeError, TypeError):
            return {"_raw": texto}
        if isinstance(envoltura, dict) and "data" in envoltura:
            return envoltura["data"]
        return envoltura

    def close(self):
        try:
            self.proc.stdin.close()
            self.proc.wait(timeout=5)
        except Exception:
            self.proc.kill()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--bin", required=True, help="el binario dark-mem-mcp a probar")
    args = ap.parse_args()

    binario = os.path.abspath(args.bin)
    if not os.path.isfile(binario):
        print("no existe el binario: %s" % binario, file=sys.stderr)
        return 2

    tmp = tempfile.mkdtemp(prefix="verifica-slot-")
    db = os.path.join(tmp, "verifica.db")
    env = dict(os.environ)
    env["DARK_DB"] = db

    m = MCP(binario, env=env)
    fallos = []
    try:
        m.initialize()

        for pid in ("alfa", "beta"):
            m.call("dark_memory_project_create",
                   {"project_id": pid, "display_name": pid.capitalize()})

        # 2. la sesion A, en "alfa"
        a = m.call("dark_memory_session_start",
                   {"operator": "verificador", "project_id": "alfa"})
        sid_a = a.get("session_id")
        print("  sesion A: %s (proyecto alfa)" % sid_a)

        # 3. la primera escritura, con el slot todavia en "alfa"
        m.call("dark_memory_agent_memory_save", {
            "operator": "verificador", "kind": "note",
            "content": "memoria uno, de la sesion A",
            "session_id": sid_a,
        })

        # 4. la sesion B en "beta": ESTO mueve el slot global
        b = m.call("dark_memory_session_start",
                   {"operator": "verificador", "project_id": "beta"})
        sid_b = b.get("session_id")
        print("  sesion B: %s (proyecto beta) — el slot global ya no esta en alfa" % sid_b)

        # 5. la segunda escritura, todavia desde la sesion A
        m.call("dark_memory_agent_memory_save", {
            "operator": "verificador", "kind": "note",
            "content": "memoria dos, de la sesion A, con el slot en beta",
            "session_id": sid_a,
        })

        # 6. cerrar la sesion A
        try:
            m.call("dark_memory_session_close", {"session_id": sid_a})
            print("  la sesion A seCerro")
        except Exception as e:
            fallos.append("la sesion A no se pudo cerrar: %s" % str(e)[:200])

        # 7. el estado real en la base
        con = sqlite3.connect("file:%s?mode=ro" % db, uri=True)
        cur = con.cursor()
        memorias = cur.execute(
            "SELECT project_id, content FROM agent_memory ORDER BY id").fetchall()
        rastros = cur.execute(
            "SELECT project_id, table_name FROM write_audit "
            "WHERE session_id = ? ORDER BY id", (sid_a,)).fetchall()
        con.close()
    except Exception as e:
        m.close()
        shutil.rmtree(tmp, ignore_errors=True)
        print("error de transporte o de la tool: %s" % str(e)[:400], file=sys.stderr)
        return 2

    m.close()

    print()
    print("  las memorias, por proyecto:")
    for pid, contenido in memorias:
        print("    %-8s %s" % (pid, contenido[:60]))
    print("  el rastro de la sesion A, por proyecto:")
    for pid, tabla in rastros:
        print("    %-8s %s" % (pid, tabla))

    # EL CONTRATO
    if len(memorias) != 2:
        fallos.append("se esperaban 2 memorias y hay %d" % len(memorias))
    for pid, contenido in memorias:
        if pid != "alfa":
            fallos.append("la memoria %r esta en el proyecto %r, se esperaba 'alfa'"
                          % (contenido[:40], pid))
    for pid, tabla in rastros:
        if pid != "alfa":
            fallos.append("el rastro de %s esta en el proyecto %r, se esperaba 'alfa'"
                          % (tabla, pid))

    print()
    if fallos:
        print("CONTRATO NO CUMPLIDO (%d):" % len(fallos))
        for f in fallos:
            print("  - %s" % f)
        shutil.rmtree(tmp, ignore_errors=True)
        return 1

    print("CONTRATO CUMPLIDO: las dos escrituras de la sesion A y su rastro "
          "estan en 'alfa', con el slot global en 'beta'.")
    shutil.rmtree(tmp, ignore_errors=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
