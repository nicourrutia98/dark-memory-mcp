# Revisión arquitectónica: la resolución del proyecto de una escritura, por la sesión que la pide

**Rama**: `fix/kernel-slot-resolution` · **Base**: `b8a19cf` (upstream/main) · **Repo**: `dark-memory-mcp`
**Alcance**: el cambio que sustituye el *slot* global mutable de proyecto activo por la sesión que pide la escritura, en las rutas de escritura y de auditoría del store, y en las rutas del orquestador que dependen de ese slot.

---

## 1. Qué cambia, fichero por fichero

| Fichero | Cambio | Qué cubre |
|---|---|---|
| `internal/store/sqlite/store.go:1122,1182,1292,1500` | `CloseSession`, `SaveHeartbeat`, `SaveResurrect`, `PromoteSessionStatus` resuelven el proyecto con `resolveWriteProject(ctx, wc)` en vez de `s.ActiveProject()` | el ciclo de vida de la sesión se ata a la sesión, no al slot |
| `internal/store/sqlite/store.go:2034,2119,2167,2262,2342,2540,2608,2664,2778,2857,2930,3124,3685,4138,4336,4391,4480,4759,5020,5233,5355,6015,6188` | el resto de los métodos de escritura del store resuelven por la sesión | ninguna escritura depende del slot |
| `internal/store/sqlite/store.go:4361` | `DeleteFrame` pasa a `DELETE FROM vibe_frames WHERE id = ? AND project_id = ?` | **antes no tenía filtro de proyecto**: el `id` es global y el borrado alcanzaba el frame de otro proyecto |
| `internal/store/sqlite/store.go:4480` | `UpdateRecallSubscriptionLastSeenToken` resuelve por la sesión en vez de `s.activeProject` | el cursor de suscripción se avanza en la fila que se creó |
| `internal/store/sqlite/store.go:349` | nuevo `resolveAuditProject(ctx, wc)`: resolución *best-effort* para las 4 tablas sin `project_id` (`research_links`, `vibe_compliance`, `constitutions`, `mods`) | su fila es global; su rastro no |
| `internal/store/sqlite/store.go:1078` | nuevo `GetSessionInProject(ctx, projectID, sessionID)` | leer una sesión sin depender del slot |
| `internal/store/sqlite/store.go` (33 eventos) · `plan.go` (2) | los eventos `audit.WriteEvent` llevan `ProjectID: projectID` | **el hueco de fondo**: el rastro caía en el proyecto del slot |
| `internal/store/sqlite/error_events.go:334` | `ResolveErrorEvent` resuelve por la sesión | un error se resuelve en el proyecto donde se registró |
| `internal/store/store.go:398` | la interfaz `Store` declara `GetSessionInProject` | contrato |
| `internal/store/postgres/store.go:1647` | `GetSessionInProject` **implementado** (no un stub) | el driver de postgres no se queda atrás |
| `internal/store/postgres/store.go` (18 eventos, 9 métodos) | el driver de postgres recibe el mismo tratamiento que sqlite | **los dos drivers se comportan igual** |
| `internal/orchestration/session_close.go:84` | el proyecto se resuelve **una vez** por la sesión nombrada y se usa en los 5 sitios que antes lo leían del slot (`ClearActiveSession`, `OnActiveSessionChanged`, la lectura de vuelta, los dos conteos) | una sola resolución por operación |
| `internal/orchestration/session_close.go:125` | la lectura de vuelta usa `GetSessionInProject` en vez de `GetSession` | `GetSession` está acotada al proyecto activo (SPEC-204 §9) y con el slot movido la sesión recién cerrada era invisible |
| `internal/orchestration/agent_memory.go:781,878` | `AgentMemoryDelegateInput` gana `SessionID`; el proyecto de la delegación se resuelve por la sesión | el contexto curado y el binding C2 caen en el proyecto de quien pide |
| `internal/orchestration/delegate_intent.go:54,177` | `DelegateIntentInput` gana `SessionID`; el proyecto se resuelve por la sesión; `prepareSubTask` propaga la sesión | lo mismo, en la ruta del router |
| `internal/orchestration/error_observatory.go:55` | `RecordError` toma el proyecto de la sesión | un error se registra donde ocurrió |
| `internal/tools/agent_memory.go:219` · `internal/tools/delegation.go:175,212` | `session_id` en el schema de `agent_memory_delegate` y de los dos schemas de `delegate_intent` | la superficie del cable puede nombrar la sesión |
| `tests/project/slot_resolution_test.go` (nuevo) | 6 tests que muerden el defecto | la evidencia |
| `tests/project/project_test.go` · `tests/orchestration/orchestrator_test.go` | dos tests preexistentes reescritos al modelo de SPEC-204 §10 | ver §5 |
| `governance/verify-slot-resolution.py` (nuevo) | verificación por el cable, con el binario real | ver §6 |

**Magnitud**: 55 métodos de escritura del store resuelven ahora por la sesión (sqlite + postgres, sin contar 19 stubs `notImpl`); 53 eventos de auditoría llevan el proyecto resuelto; **0** métodos de escritura leen el slot.

---

## 2. Mapa de invariantes

| Invariante | Enunciado (resumido) | ¿Se cumple? | Cómo |
|---|---|---|---|
| **INV-1** (write-path audit) | todo `Save*` inserta su fila de `write_audit` en la **misma transacción** que la escritura de datos | **Sí, y mejor** | La fila de auditoría sigue en la misma transacción, y ahora lleva además el `project_id` correcto: el `WriteContext` que INV-1 ya definía (`{Actor, SessionID, WritePath, ConstitutionID, ConstitutionVersion, ProjectID}`) es el que se propaga. El campo `ProjectID` existía en el contrato y ningún evento lo rellenaba: el invariante estaba **declarado y no cumplimentado**. |
| **INV-2** (per-session scoping) | el `Recall` filtra por `SessionScope` | **Sí** | No se toca el camino de lectura de memoria; solo cambia de qué proyecto se resuelve. |
| **INV-7** (per-project scoping) | *"Every tenant-scoped table carries `project_id` … `Store.Save*` and reads filter by `ActiveProject()`"* | **La intención, sí. El mecanismo literal, no — y hay que actualizar el texto** | El enunciado nombra `ActiveProject()` como el filtro. Este cambio lo sustituye por el proyecto que resuelve la sesión, que es lo que SPEC-204 §8 depreca como fuente de resolución. **El texto de `docs/INVARIANTS.md:169` queda desfasado** y hay que enmendarlo. Es el hallazgo P1 de §7. |
| **INV-8** (per-MCP database isolation) | el aislamiento duro es la base de datos por MCP | **Sí** | No se toca. Este cambio no amplía ni reduce el aislamiento duro: mueve datos *dentro* de la misma base. |
| **INV-9** (reservado) | — | **N/A** | Reservado. |
| **INV-10** (las memorias sobreviven al ciclo de vida de la sesión) | las filas de `agent_memory` se consultan por `(project_id, agent_id, kind)` | **Sí, y con menos ruido** | Las filas no se mueven ni se duplican; lo que cambia es que ahora caen en el proyecto que les corresponde, con lo que la consulta por `(project_id, agent_id, kind)` deja de mezclar proyectos. |
| **INV-19** (namespace primitive) | *"`project_id` is a SOFT workstream namespace (filter column), NOT a multi-tenant primitive"* | **Sí, y este cambio lo respeta** | Nada de lo que se hace aquí pretende hacer de `project_id` una frontera de seguridad. Lo que se arregla es la **corrección del espacio de nombres**: el rastro de una escritura estaba en el espacio de nombres equivocado. Ver §5. |
| **INV-12** (cadena HMAC) | la cadena de auditoría es por proceso | **No aplica** | `write_audit` no forma parte de la cadena HMAC (`audit_export` está roto en esta build y usa su propia tabla). |
| **INV-11, INV-13, INV-14, INV-15** | token de capacidad, redacción, PII, inyección | **No aplica** | Ninguna de las rutas tocadas deals con tokens, redacción o PII. |

**El contrato de la constitution** (`dark-agents/workspace-policy` v1.4.1): el cambio no altera la constitution, ni su SHA, ni el versionado. El §7 de `ARCHITECTURE-V4.md` (contrato de constitution) sigue vigente. La constitution sigue siendo la que gobierna el ciclo y el `WriteContext` sigue llevando `ConstitutionID`/`ConstitutionVer` en cada escritura.

**§12 (disciplina de tests)**: el cambio añade una capa **contractual** (los tests de `tests/project/`, que son la suite de aislamiento entre proyectos, según la propia INV-7) y una capa de **extremo a extremo** (`governance/verify-slot-resolution.py`, por el cable, con el binario real). No añade property tests sobre invariantes, que es lo que §12.1 pide para las 15 INV — **es un hueco preexistente del repo**, no de este cambio (`tests/invariants/` tiene 2 ficheros, no 15; `tests/security/` no existe). El §12.2 (mutation testing) no se ha ejecutado en este cambio.

---

## 3. El modelo de la frontera de confianza (SPEC-204 §10)

Este es el punto donde el cambio se mueve a sí mismo, y hay que decirlo sin adornos.

**Lo que SPEC-204 §10 dice** (`docs/specs/SPEC-204-resolucion-central-de-la-identidad.md:230`):

> Al resolver la sesión **por id** y no por el proyecto activo, **nombrar la sesión de otro proyecto pasa a ser una identidad válida** para las 78 tools. Antes se rechazaba («identity unavailable for this session»).

**Lo que este cambio hace con ese modelo**:

1. **Lo aplica en el store.** `CloseSession` con un `WriteContext` de un proyecto y el `session_id` de una sesión de otro proyecto cierra **esa** sesión: el `UPDATE` lleva `WHERE session_id = ? AND project_id = ?` con el proyecto **resuelto desde la sesión**, no el del `WriteContext`. Antes el `WHERE` llevaba el proyecto del `WriteContext` (= el slot) y no casaba con ninguna fila.

2. **Lo aplica en el orquestador.** `session_close.go:84` resuelve el proyecto desde la sesión nombrada y lo usa en los cinco sitios siguientes. Una operación que nombra su sesión ya no la ejecuta en el proyecto del slot.

3. **Deja `GetSession` donde estaba.** SPEC-204 §9: *"`GetSession` conserva su filtro por proyecto activo: INV-7 no se relaja"*. `GetSession` **no se ha tocado**. Lo que se añade es `GetSessionInProject`, un hermano *scoped*, con el mismo patrón que `GetAgentMemoryInProject` (SPEC-203). La lectura que el orquestador necesitaba —leer la sesión que acaba de cerrar— se hace por la vía nueva, no<Task> relajando la vieja.

**La puerta de al revés, y por qué está cerrada.** El arreglo amplía qué puede hacer un `session_id` nombrado, así que la pregunta obligatoria es si abre la puerta contraria: *¿puede una sesión de un proyecto tocar las filas de otro?*

- **No, en las escrituras por fila.** Todas las escrituras por fila llevan ahora `AND project_id = <proyecto resuelto>`, y el proyecto resuelto sale de la sesión. `DeleteFrame`, que era el peor caso (no filtraba por proyecto en absoluto), queda acotado en `store.go:4361`.
- **No, en las lecturas.** `GetSessionInProject` devuelve `(nil, nil)` —no un error— cuando la fila está en otro proyecto: el cruce se comporta como "no existe", igual que `GetAgentMemoryInProject`.
- **Sí, en un sentido que es deliberado y está documentado**: nombrar la sesión de otro proyecto es una identidad válida. Eso es SPEC-204 §10, no este cambio. Lo que este cambio hace es **dejar de fingir que el modelo anterior se cumplía**: antes, el slot decidía y la frontera se mantenía por accidente (el `UPDATE` no casaba). Ahora la frontera se mantiene **a propósito**, por la razón correcta.

**El coste honesto de esto**: el `session_id` es ahora, en el store, la credencial que decide el proyecto. SPEC-204 §10 ya lo dice (128 bits de entropía, `ListSessions` filtrado por proyecto, no se acepta de un argumentario sin el scope del operador). Este cambio no añade una credencial nueva: hace que la existente se use como la spec dice que se use. Aun así, es un cambio de comportamiento observable y el operador tiene que saberlo.

---

## 4. Lo que **no** cambia

- `GetSession` sigue acotada al proyecto activo.
- El *slot* global sigue existiendo y sigue siendo el último recurso de la cadena (`resolveWriteProject`: sesión → `wc.ProjectID` → slot). No se elimina: eliminarla es trabajo de otra spec.
- La frontera de aislamiento **dura** no cambia: sigue siendo la base de datos por MCP (INV-19).
- Los 19 métodos `notImpl` del driver de postgres siguen sin implementar.
- La API pública de las tools solo crece: `session_id` es un campo **opcional** en `agent_memory_delegate` y `delegate_intent`. Ninguna tool cambia de firma ni de comportamiento required.

---

## 5. Verificación

### 5.1 Por el cable, con el binario real — la evidencia principal

`governance/verify-slot-resolution.py` habla JSON-RPC 2.0 por stdio con el binario construido y comprueba el estado en la base. El escenario es el que reproduce el defecto: **dos sesiones en el mismo proceso, de proyectos distintos**.

| Binario | Resultado |
|---|---|
| **Sin el arreglo** (el instalado, construido el 2026-11-17 22:42) | la sesión A **no se puede cerrar** (`ErrNotFound`); **2 filas de `write_audit` caen en `beta`** en vez de `alfa` |
| **Con el arreglo** (construido desde este worktree) | las dos escrituras y **todo** su rastro en `alfa`, con el slot en `beta`; la sesión A se cierra. `exit 0` |

Dato que delimita el alcance y que conviene no maquillar: con el binario **sin** arreglo, las **filas de datos** sí caían en `alfa` (`SaveAgentMemory` ya resolvía por sesión). El defecto estaba en el **rastro** y en el **ciclo de vida de la sesión**, no en el dato. Eso es exactamente lo que dice la medición siguiente.

### 5.2 La medición, en la base de datos viva

Con la base de producción de esta máquina:

- **182 de 21.185 filas de `write_audit` (0,9%)** con `project_id` distinto al de su sesión: `vibe_frames` 72, `error_events` 55, `vibe_specs` 21, `vibe_drift_reports` 17, `vibe_artifacts` 15, `sdd_evaluations` 2.
- El **0,9%** está por debajo del umbral que el propio INV-1 fija como señal de operador (*"write_audit row count should grow ≈ data row count. Discrepancy > 1% = bug"*, `docs/INVARIANTS.md:34`): la señal que el repo ya tenía detectaba el síntoma sin señalar la causa.

### 5.3 Los tests

- **Muerden**: se comprobó que `TestSpec204_SecondVuelta_LaFilaDeAuditoriaAterrizaEnSuProyecto` **falla** contra el código anterior (con el `ProjectID` de los eventos retirado, la fila cae en `globex` — el síntoma exacto de producción) y **pasa** con el arreglo.
- **Cubren los dos lados**: `TestSpec204_SecondVuelta_NoSeAbreLaPuertaDeAlReves` y `TestSpec204_SecondVuelta_NoSeCierraLaSesionDeOtro` comprueban que la sesión de `acme` **no** puede tocar el artefacto ni la sesión de `globex`.
- `go test ./... -count=1`: **pasa**, salvo el fallo preexistente de cgo en `internal/embedder/onnx` (build constraints de `onnxruntime_go`; idéntico en `upstream/main`, **no lo introduce este cambio**).
- **Wire gate** (`tests/wire/...`, la puerta de producción): **pasa** con el binario real construido.
- **Governance**: `governance/check-constitution.sh` y `governance/test-dm-flow.sh` pasan; `scripts/lint-no-private-projects.sh` pasa (exit 0).

### 5.4 Lo que **no** se ha verificado

- **El driver de postgres no se ha ejecutado contra un postgres vivo.** El CI lo hace (`tests/dual_driver/`, en el job "postgres smoke"); aquí no había postgres. El código compila y el resto del driver recibe el mismo tratamiento, pero **el comportamiento en postgres está sin medir**.
- **No se ha ejecutado mutation testing** (§12.2). Con la doc de §15 del repo, la herramienta real es `gremlins v0.6.0`, no `go-mutesting`.
- **No hay property tests** sobre las INV afectadas (§12.1).
- **El cambio no está en `main`**: no se ha medido el efecto sobre el servidor que está corriendo ahora (que es un binario anterior).

---

## 6. Los dos tests preexistentes que hubo que reescribir

Dos tests fallaban con el arreglo. No eran tests rotos: codificaban el modelo **anterior** a SPEC-204 §10.

| Test | Antes | Ahora |
|---|---|---|
| `TestProject_CloseSession_CrossProject_NotClosed` (`tests/project/project_test.go`) |cerrar la sesión de otro proyecto devuelve `ErrNotFound` y **no** la muta | `TestProject_CloseSession_ScopedToTheNamedSession`: el cierre afecta **solo** a la sesión nombrada; la sesión del otro proyecto queda intacta; **el rastro del cierre cae en el proyecto de la sesión**; una sesión inexistente sigue dando `ErrNotFound` |
| `TestSessionClose_CrossProject` (`tests/orchestration/orchestrator_test.go`) | cerrar desde otro proyecto falla con `ErrNotFound` (el filtro de proyecto lo bloquea) | el cierre nombra la sesión y la cierra, con el slot en otro proyecto; la otra sesión no se toca; el rastro cae en el proyecto de la sesión; el doble cierre sigue dando `ErrNotFound` |

**La intensión del test viejo se conserva y se refuerza**: lo que el test viejo protegía —*que un cierre no toque sesiones ajenas*— sigue protegido, y ahora se comprueba además *que el rastro no se vaya al proyecto ajeno*, que era el hueco real.

**El otro test que protege `GetSession` —`TestProject_GetSession_CrossProject_ReturnsNil`— no se ha tocado y sigue verde**: `GetSession` sigue acotada al proyecto activo. Por eso el cambio añade `GetSessionInProject` en vez de relajar `GetSession`.

---

## 7. Hallazgos honestos

### P1 — El texto de INV-7 queda desfasado y hay que enmendarlo

`docs/INVARIANTS.md:169` dice: *"`Store.Save*` and reads filter by `ActiveProject()`"*. Este cambio sustituye ese mecanismo por el proyecto resuelto desde la sesión. La **intención** del invariante (aislamiento por proyecto, rechazo de proyectos desconocidos) se cumple y se cumple mejor; el **mecanismo literal** ya no es el que el texto nombra. Un invariante cuyo texto no describe lo que el código hace es un invariante que deja de ser un invariante: o se enmenda el texto, o el próximo que lo lea creerá algo falso.

**Acción**: enmendar `docs/INVARIANTS.md` §INV-7 para que nombre la cadena de resolución (sesión → `wc.ProjectID` → slot) en lugar de `ActiveProject()`, citando SPEC-204 §8.

### P1 — El texto de INV-19 obliga a no vender esto como una corrección de seguridad

`docs/INVARIANTS.md:320` es explícito: *"`project_id` is a SOFT workstream namespace (filter column), NOT a multi-tenant primitive. HARD isolation is `coexistence_group`"*. Por tanto **este cambio no cierra una vulnerabilidad de multi-tenant**: los datos siempre estuvieron en la misma base, que es el único aislamiento que había. Lo que arregla es la **corrección del espacio de nombres** y la **atribución del rastro**.

La consecuencia practica: el rastro de una escritura ya no se puede usar como prueba de "quién escribió esto en mi proyecto" si el proyecto no es de fiar. Esto mejora, pero no crea, una garantía forense. **Quien presente este cambio debe decirlo así.**

### P1 — `GetSessionInProject` en postgres se implementó, no se stubbeó, y hace falta decirlo

La primera versión de este cambio dejó `GetSessionInProject` como `notImpl` en el driver de postgres, por analogía con los métodos de plan, que sí son stub. Eso era un **error**: `GetSession` **sí** está implementado en postgres, y `session_close` lee la sesión de vuelta justo después de cerrarla. Con el stub, `session_close` habría fallado con *"not implemented"* en **toda** instalación de postgres — una regresión introducida por el arreglo que el arreglo pretendía eliminar. Se implementó (`internal/store/postgres/store.go:1647`).

**Regla que se sigue de aquí**: un stub no es una neutralidad, es un fallo silencioso que se parece a una función. Si el método equivalente existe en el driver, el nuevo se implementa.

### P2 — El *fallback* al slot sigue existiendo en dos sitios, y es intencionado

`resolveWriteProject` termina en `s.ActiveProject()` cuando no hay sesión ni `wc.ProjectID` (SPEC-204 §8: el slot es el último recurso). Y `recordWriteLockedTx` sigue rellenando `ev.ProjectID` con el slot si el evento llega vacío. Las dos cosas son deliberadas y están comentadas, pero **son el resto del agujero**: un llamante que no nombre sesión y no lleve `project_id` sigue Writing en el proyecto del slot. El cambio reduce el alcance de esa condición; no la elimina.

### P2 — `resolveAuditProject` es *best-effort* y, cuando falla, el rastro vuelve al slot

Para las 4 tablas globales (`research_links`, `vibe_compliance`, `constitutions`, `mods`) la resolución del proyecto del rastro no puede fallar la escritura: la fila es global y la escritura es válida de todos modos. Cuando la resolución falla, el rastro vuelve al slot. Es el comportamiento anterior, y es la única cosa que se ha dejado deliberadamente igual.

### P2 — Dos tools aceptan `session_id` pero el servidor no lo exige

`agent_memory_delegate` y `delegate_intent` declaran `session_id` como **opcional**, y cuando no se nombra, caen a la sesión activa del proyecto activo (el slot). Quien llama puede seguir viendo el comportamiento viejo si no pasa la sesión. La **documentación del schema** lo dice; el **gate** no lo impone. Es coherente con el resto de las tools que ya aceptan `session_id` (`agent_memory_save`, `agent_memory_recall`), pero conviene que el operador sepa que **nombrar la sesión es lo que activa la corrección**.

### P2 — La puerta de al revés está cerrada por el `WHERE`, no por el gate

El aislamiento entre proyectos de las escrituras por fila lo garantiza el `WHERE project_id = ?` del store, no el gate del servidor. Eso es lo correcto (el store es la frontera), pero significa que **un `Store` mal implementado basta para cruzar proyectos**: el store es la única defensa. Este cambio refuerza el store en 55 métodos; no añade una segunda capa.

### P3 — `DeleteFrame` cambia de comportamiento observable

Antes, `DeleteFrame` borraba **cualquier** frame por id. Ahora solo borra el del proyecto resuelto. Es una corrección (el borrado sin filtro de proyecto era un agujero), pero **cambia el comportamiento**: un llamante que nombra un frame de otro proyecto ahora recibe `ErrNotFound` donde antes borraba. No se ha encontrado ningún llamante legítimo que dependa del comportamiento viejo, pero es un cambio de contrato.

### P3 — `internal/store/sqlite/store.go` está en CRLF

El fichero está en CRLF en `upstream/main`. Un `gofmt -w` lo convierte a LF entero y genera un diff de 12.000 líneas que hides el cambio real. Se restauró el CRLF: el diff de este fichero son 300 líneas. **Aviso para el que cherry-pickee**: no corras `gofmt -w` sobre ese fichero.

---

## 8. Veredicto global

**El cambio se puede aterrizar**, con dos condiciones:

1. **Enmendar el texto de INV-7** (`docs/INVARIANTS.md:169`) para que nombre la cadena de resolución y no `ActiveProject()`. Sin eso, el repo queda con un invariante cuyo texto describe un mecanismo que ya no existe.
2. **Medir el driver de postgres contra un postgres vivo** antes de mergear. El código está y compila, pero el comportamiento no está medido, y el CI tiene el job (`tests/dual_driver/`) que lo mide: dejarlo pasar por el CI es exactamente lo que hay que hacer.

**Lo que el cambio resuelve, medido**: el rastro de las escrituras deja de caer en el proyecto equivocado (182 de 21.185 filas, 0,9%, en la base viva); las sesiones se pueden cerrar aunque otra sesión haya movido el slot (8 sesiones atascadas en producción); `DeleteFrame` deja de alcanzar frames de otro proyecto; el contexto de delegación y el Error Observatory dejan de ver lo de otro proyecto.

**Lo que el cambio NO es**: una corrección de seguridad. El aislamiento duro de este sistema es la base de datos por MCP (INV-19); `project_id` es un espacio de nombres. Este cambio hace que el espacio de nombres se respete.

**Lo que queda fuera, y está declarado**: el *fallback* al slot (P2), el driver de postgres sin medir (§5.4), y los dos tests preexistentes reescritos (§6) — que es una decisión de modelo, no un arreglo de tests, y que por eso lleva su propia memoria.
