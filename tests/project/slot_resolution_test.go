// Package project_test — SPEC-204, la segunda vuelta: las escrituras que
// NO usaban la cadena de resolucion.
//
// EL BUG QUE ESTE TEST FIXES. SPEC-204 (PR #39, commit 044fc8d) arreglo que
// una escritura se resuelva por la SESION que la pide y no por el slot
// global. Se arreglo en el sitio que mas duele: el orquestador (resolveCallProject
// se tragaba el error) y en los metodos del store que ya usaban
// resolveWriteProject.
//
// Pero el arreglo fue PARCIAL, y la parte que falto es la que se ve aqui.
// Estos metodos del store toman un WriteContext con SessionID y, aun asi,
// resuelven el proyecto con s.ActiveProject() — el slot global mutable:
//
//	CloseSession             SaveHeartbeat          SaveResurrect
//	PromoteSessionStatus     UpdateSpec             DeleteSpec
//	DeleteBrandGuide         UpdateArtifact         DeleteArtifact
//	SetArtifactValidation    SaveFrame              SaveRecallSubscription
//
// El modo de fallo es el mismo en todos: la fila se CREA o se IDENTIFICA en
// el proyecto de la sesion (porque el INSERT o el SELECT previo usan la
// cadena correcta) y luego la operacion que la completa se hace en el
// proyecto del slot. Si otra sesion movio el slot entre medias, el UPDATE no
// casa con ninguna fila.
//
// MEDIDO el 2026-11-18 en produccion, con este repositorio:
//
//	· SetArtifactValidation: el artefacto 558 quedo con validation_status
//	  "pending" cuando su veredicto fue "drift_detected" (debia quedar
//	  "failed"), y el razonamiento del juez acababa con
//	  "; set validation=failed failed: sql: no rows in result set". El
//	  artefacto 560, publicado tres segundos despues por otra sesion, si
//	  quedo bien. El fallo es INTERMITENTE y depende del timing de otra
//	  sesion: el peor tipo, porque parece que no existe.
//
//	· CloseSession: ocho sesiones quedaron abiertas y no se podian cerrar,
//	  con "identity unavailable for this session" en el Error Observatory.
//	  Motivo: el UPDATE lleva "AND project_id = <slot>", y con el slot en
//	  otro proyecto no casa ninguna fila.
//
// QUE ES PEOR EN UN CASO QUE EN OTRO. Para SetArtifactValidation y
// SaveFrame el resultado es una fila a medio escribir. Para CloseSession el
// resultado es que la sesion NO SE PUEDE CERRAR: cada intento devuelve
// "identity unavailable for this session" y las sesiones se acumulan abiertas
// hasta que el sweeper las recoge. Un sistema que no puede cerrar sus
// unidades de trabajo no puede saber si termino.
//
// LA FORMA DEL TEST. Un store real de sqlite con dos proyectos, una sesion en
// "acme" y el slot global apuntando a "globex" (que es lo que hace otra
// sesion del mismo proceso MCP). Se ejercita cada operacion de la lista con
// el WriteContext de la sesion de "acme" y se comprueba UNA cosa: que la
// operacion SURTE EFECTO. Si la operacion se dirige al proyecto equivocado,
// el UPDATE no casa con ninguna fila y la operacion falla con ErrNotFound —
// que es exactamente lo que se ve en produccion.
//
// Cada caso del test se ha comprobado que FALLA contra el codigo anterior y
// PASA con el arreglo.
package project_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dark-agents/dark-memory-mcp/internal/agentmemory"
	"github.com/dark-agents/dark-memory-mcp/internal/atomic"
	"github.com/dark-agents/dark-memory-mcp/internal/project"
	"github.com/dark-agents/dark-memory-mcp/internal/session"
	"github.com/dark-agents/dark-memory-mcp/internal/store"
	"github.com/dark-agents/dark-memory-mcp/internal/store/runtime"
	"github.com/dark-agents/dark-memory-mcp/internal/vibeflow"
)

// labStore es el store real con dos proyectos, una sesion en "acme" y el
// slot global en "globex".
type labStore struct {
	store.Store
	t *testing.T
}

// newLabStore abre el store y reproduce el escenario: el slot global apunta
// a un proyecto que NO es el de la sesion.
//
// Que el slot apunte a otro proyecto no es un montage: es lo que hace otra
// sesion del mismo proceso MCP. El slot global es un unico mutable
// compartido por todos los clientes del proceso (MEDIDO: asifallen tres
// memorias de una sesion a un proyecto ajeno).
func newLabStore(t *testing.T) *labStore {
	t.Helper()
	ctx := context.Background()

	cfg := store.Config{
		Driver:      store.DriverSQLite,
		DSN:         filepath.Join(t.TempDir(), "test.db"),
		WALMode:     true,
		ForeignKeys: true,
		BusyTimeout: 5 * time.Second,
	}
	real, err := runtime.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })

	if err := real.CreateProject(ctx, &project.Project{ProjectID: "acme", DisplayName: "ACME"}); err != nil {
		t.Fatalf("create acme: %v", err)
	}
	if err := real.CreateProject(ctx, &project.Project{ProjectID: "globex", DisplayName: "Globex"}); err != nil {
		t.Fatalf("create globex: %v", err)
	}

	// El slot global: lo mueve OTRA sesion del proceso.
	real.SetActiveProject(ctx, "globex")

	// La sesion del que escribe: es de "acme".
	wc := store.WriteContext{Actor: "test", SessionID: "sess-A", WritePath: "test", ProjectID: "acme"}
	if _, err := real.SaveSession(ctx, wc, &session.Session{SessionID: "sess-A", Status: string(session.StatusOpen)}); err != nil {
		t.Fatalf("save session A: %v", err)
	}
	return &labStore{Store: real, t: t}
}

// newSessionA crea una sesion nueva de "acme" y devuelve su id.
//
// Cada caso necesita la suya: cerrar una sesion la deja en status "closed",
// y el latido (y el estado de la sesion) solo se pueden tocar si esta
// "open" o "idle". Compartir una sesion entre casos hacia que el caso
// segundo fallara por el primero, que es un fallo del test y no del kernel.
func (l *labStore) newSessionA(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	id := "sess-A-" + t.Name()
	wc := store.WriteContext{Actor: "test", SessionID: id, WritePath: "test", ProjectID: "acme"}
	if _, err := l.SaveSession(ctx, wc, &session.Session{SessionID: id, Status: string(session.StatusOpen)}); err != nil {
		t.Fatalf("save session %s: %v", id, err)
	}
	return id
}

// wcA es el WriteContext de la sesion de "acme": la que escribe.
func wcA() store.WriteContext {
	return store.WriteContext{
		Actor:     "test",
		SessionID: "sess-A",
		WritePath: "test",
		ProjectID: "acme",
	}
}

// El caso que se repite: una operacion que se dirige a una fila por id
// (o a la sesion por id) con el WriteContext de la sesion de "acme", con el
// slot global en "globex".
//
// Antes del arreglo: la operacion se ejecutaba contra "globex", no casaba con
// ninguna fila y devolvia ErrNotFound — el "identity unavailable for this
// session" de produccion.
//
// Despues: la operacion se dirige al proyecto de la sesion y surge efecto.
func TestSpec204_SecondVuelta_LasEscriturasDelSlot(t *testing.T) {
	ctx := context.Background()
	s := newLabStore(t)

	t.Run("CloseSession", func(t *testing.T) {
		// La sesion que hay que cerrar es la de "acme".
		if err := s.CloseSession(ctx, wcA(), "sess-A", "clean"); err != nil {
			t.Fatalf("CloseSession: %v; la sesion es de acme y el WriteContext lo dice: la operacion no puede quedar en el slot global", err)
		}
	})

	t.Run("SaveHeartbeat", func(t *testing.T) {
		sid := s.newSessionA(t)
		wc := store.WriteContext{Actor: "test", SessionID: sid, WritePath: "test", ProjectID: "acme"}
		if err := s.SaveHeartbeat(ctx, wc, sid); err != nil {
			t.Fatalf("SaveHeartbeat: %v; el latido es de la sesion de acme, no del slot global", err)
		}
	})

	t.Run("SetArtifactValidation", func(t *testing.T) {
		// El artefacto se CREA en acme (SaveArtifact ya resuelve por sesion).
		artID, err := s.SaveArtifact(ctx, wcA(), &vibeflow.Artifact{
			ArtifactType: "text",
			ArtifactURL:  "file:///tmp/x",
		})
		if err != nil {
			t.Fatalf("SaveArtifact: %v", err)
		}
		// Y luego su estado de validacion se actualiza: con el codigo
		// anterior, ese UPDATE iba al proyecto del slot.
		if err := s.SetArtifactValidation(ctx, wcA(), artID, "passed"); err != nil {
			t.Fatalf("SetArtifactValidation: %v; el artefacto es de acme y el estado tiene que quedar en acme", err)
		}
		// Y el estado tiene que SER ese, no "pending".
		got := artifactValidation(t, s, artID)
		if got != "passed" {
			t.Fatalf("SetArtifactValidation: el estado del artefacto es %q, se esperaba \"passed\"", got)
		}
	})
}

// artifactValidation lee el estado de validacion de un artefacto.
//
// La lectura ES por proyecto: el filtro del store comprueba contra el
// proyecto activo (INV-7), asi que el slot se apunta al proyecto del
// artefacto antes de leer. Sin eso la lectura se rechaza con el error de
// proyecto cruzado, que es el store trabajando bien y no un fallo de lo que
// se esta probando.
func artifactValidation(t *testing.T, s store.Store, id int64) string {
	t.Helper()
	ctx := context.Background()
	s.SetActiveProject(ctx, "acme")
	var got string
	err := s.RawDB().QueryRowContext(ctx,
		`SELECT validation_status FROM vibe_artifacts WHERE id = ?`, id).Scan(&got)
	if err != nil {
		t.Fatalf("leer validation_status: %v", err)
	}
	return got
}

// El otro lado del contrato: una escritura NO puede tocar la fila de otro
// proyecto aunque el slot apunte ahi. El arreglo no puede abrir esa puerta
// para arreglar la otra.
//
// MEDIDO: con el slot en "globex" y la sesion de "acme", el
// SetArtifactValidation sobre un artefacto de GLOBEX debe fallar, no
// actualizarlo.
func TestSpec204_SecondVuelta_NoSeAbreLaPuertaDeAlReves(t *testing.T) {
	ctx := context.Background()
	s := newLabStore(t)

	// Un artefacto de globex.
	s.SetActiveProject(ctx, "globex")
	wcG := store.WriteContext{Actor: "test", SessionID: "sess-G", WritePath: "test", ProjectID: "globex"}
	if _, err := s.SaveSession(ctx, wcG, &session.Session{SessionID: "sess-G", Status: string(session.StatusOpen)}); err != nil {
		t.Fatalf("save session G: %v", err)
	}
	gArtID, err := s.SaveArtifact(ctx, wcG, &vibeflow.Artifact{
		ArtifactType: "text",
		ArtifactURL:  "file:///tmp/g",
	})
	if err != nil {
		t.Fatalf("SaveArtifact (globex): %v", err)
	}

	// Ahora el slot vuelve a "globex" (lo pone la otra sesion) y se escribe
	// desde la sesion de "acme".
	s.SetActiveProject(ctx, "globex")
	err = s.SetArtifactValidation(ctx, wcA(), gArtID, "passed")
	if err == nil {
		t.Fatalf("SetArtifactValidation: la sesion de acme actualizó un artefacto de globex; el arreglo de la resolucion NO puede abrir la puerta de al reves")
	}
}

// Lo mismo, para el cierre de sesion: la sesion de "globex" no se puede
// cerrar desde el WriteContext de "acme".
func TestSpec204_SecondVuelta_NoSeCierraLaSesionDeOtro(t *testing.T) {
	ctx := context.Background()
	s := newLabStore(t)

	s.SetActiveProject(ctx, "globex")
	wcG := store.WriteContext{Actor: "test", SessionID: "sess-G", WritePath: "test", ProjectID: "globex"}
	if _, err := s.SaveSession(ctx, wcG, &session.Session{SessionID: "sess-G", Status: string(session.StatusOpen)}); err != nil {
		t.Fatalf("save session G: %v", err)
	}
	s.SetActiveProject(ctx, "globex")

	if err := s.CloseSession(ctx, wcA(), "sess-G", "clean"); err == nil {
		t.Fatalf("CloseSession: la sesion de globex se cerro desde el WriteContext de acme; la frontera de proyecto se ha roto")
	}
}

// La memoria: el caso que se vio en produccion tres veces. Una escritura de
// memoria desde la sesion de "acme", con el slot en "globex", tiene que
// LANDAR en acme.
func TestSpec204_SecondVuelta_LaMemoriaAterrizaEnSuProyecto(t *testing.T) {
	ctx := context.Background()
	s := newLabStore(t)

	if _, err := s.SaveAgentMemory(ctx, wcA(), &agentmemory.AgentMemory{
		Operator: "test",
		Kind:     "note",
		Content:  "esta memoria es de acme",
	}); err != nil {
		t.Fatalf("SaveAgentMemory: %v", err)
	}

	if n := countMem(t, s, "acme"); n != 1 {
		t.Errorf("SaveAgentMemory: hay %d memoria(s) en acme, se esperaba 1", n)
	}
	if n := countMem(t, s, "globex"); n != 0 {
		t.Errorf("SaveAgentMemory: %d memoria(s) aterrizaron en globex (el slot global); la sesion es de acme", n)
	}
}

// LA FILA DE AUDITORIA: el hueco medido del 0,9%.
//
// El bug mas profundo de la familia no era que la escritura aterrizara en el
// proyecto equivocado: era que el RASTRO de la escritura aterrizara en el
// proyecto equivocado, aunque la escritura cayera en el sitio correcto.
//
// MEDIDO el 2026-11-18 en la base de datos viva de este repositorio:
//
//	182 de 21.185 filas de write_audit (0,9%) tienen un project_id que NO es
//	el de su sesion.
//
// Por tabla: vibe_frames 72, error_events 55, vibe_specs 21,
// vibe_drift_reports 17, vibe_artifacts 15, sdd_evaluations 2.
//
// El motivo era uno solo, y estaba en el store: el evento de auditoria se
// rellenaba con el proyecto del SLOT cuando el evento no traia ProjectID.
// Ningun metodo de escritura lo traia — ni siquiera los que ya resolvian la
// sesion correctamente. Es decir: el store resolvia bien la fila y despues
// resolvia MAL su rastro.
//
// Por que importa mas que la fila: el rastro es lo que se consulta para
// reconstruir que paso. Un rastro en el proyecto equivocado no es un
// problema de orden: es un problema de ATRIBUCION. Un proyecto no puede
// defenderse de un rastro que no tiene, y el proyecto ajeno carga con
// rastros que no son suyos.
//
// Este test muerde el hueco: una escritura desde la sesion de "acme", con el
// slot en "globex", y la fila de write_audit tiene que estar en "acme".
func TestSpec204_SecondVuelta_LaFilaDeAuditoriaAterrizaEnSuProyecto(t *testing.T) {
	ctx := context.Background()
	s := newLabStore(t)

	memID, err := s.SaveAgentMemory(ctx, wcA(), &agentmemory.AgentMemory{
		Operator: "test",
		Kind:     "note",
		Content:  "esta memoria es de acme, y su rastro tambien",
	})
	if err != nil {
		t.Fatalf("SaveAgentMemory: %v", err)
	}

	// La fila de auditoria de ESA memoria.
	var auditProject string
	err = s.RawDB().QueryRowContext(ctx,
		`SELECT project_id FROM write_audit WHERE table_name = 'agent_memory' AND row_id = ? ORDER BY id DESC LIMIT 1`,
		memID).Scan(&auditProject)
	if err != nil {
		t.Fatalf("leer la fila de auditoria de la memoria %d: %v", memID, err)
	}
	if auditProject != "acme" {
		t.Fatalf("la fila de auditoria de la memoria %d esta en el proyecto %q, se esperaba %q: el rastro de una escritura no puede aterrizar en el proyecto del slot global", memID, auditProject, "acme")
	}

	// Y lo mismo para el artefacto: su INSERT y su UPDATE de validacion
	// tienen que dejar el mismo rastro, en el mismo proyecto.
	artID, err := s.SaveArtifact(ctx, wcA(), &vibeflow.Artifact{
		ArtifactType: "text",
		ArtifactURL:  "file:///ejemplo.txt",
	})
	if err != nil {
		t.Fatalf("SaveArtifact: %v", err)
	}
	if err := s.SetArtifactValidation(ctx, wcA(), artID, "passed"); err != nil {
		t.Fatalf("SetArtifactValidation: %v", err)
	}
	var artAuditProject string
	err = s.RawDB().QueryRowContext(ctx,
		`SELECT project_id FROM write_audit WHERE table_name = 'vibe_artifacts' AND row_id = ? ORDER BY id DESC LIMIT 1`,
		artID).Scan(&artAuditProject)
	if err != nil {
		t.Fatalf("leer la fila de auditoria del artefacto %d: %v", artID, err)
	}
	if artAuditProject != "acme" {
		t.Fatalf("la fila de auditoria del artefacto %d esta en el proyecto %q, se esperaba %q", artID, artAuditProject, "acme")
	}

	// Y la sesion misma: su fila de auditoria tambien.
	var sessAuditProject string
	err = s.RawDB().QueryRowContext(ctx,
		`SELECT project_id FROM write_audit WHERE table_name = 'sessions' AND session_id = 'sess-A' ORDER BY id DESC LIMIT 1`,
	).Scan(&sessAuditProject)
	if err != nil {
		t.Fatalf("leer la fila de auditoria de la sesion: %v", err)
	}
	if sessAuditProject != "acme" {
		t.Fatalf("la fila de auditoria de la sesion esta en el proyecto %q, se esperaba %q", sessAuditProject, "acme")
	}
}

// countMem cuenta las memorias de un proyecto. El filtro es un narrowing que
// el store comprueba contra el proyecto activo (INV-7), asi que el slot se
// apunta al proyecto que se cuenta antes de leer.
func countMem(t *testing.T, s store.Store, projectID string) int {
	t.Helper()
	ctx := context.Background()
	s.SetActiveProject(ctx, projectID)
	rows, err := s.ListAgentMemory(ctx, agentmemory.AgentMemoryListFilters{
		ProjectID: projectID,
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("list agent memory (%s): %v", projectID, err)
	}
	return len(rows)
}

// El marco atomico: SaveFrame tiene la misma forma que SetArtifactValidation
// (crea una fila y la resuelve por el slot), asi que se comprueba tambien.
func TestSpec204_SecondVuelta_ElFrameAterrizaEnSuProyecto(t *testing.T) {
	ctx := context.Background()
	s := newLabStore(t)

	// El expiry es obligatorio para que el frame sea VISIBLE: ListFrames
	// filtra por "expires_at > ?", y un frame sin expiry es invisible por
	// construccion (no es un fallo de la resolucion del proyecto).
	frame := &atomic.FrameEnvelope{
		SessionID:  "sess-A",
		ScopeLevel: atomic.ScopeProject,
		ScopeID:    "acme",
		Kind:       atomic.FrameIdentity,
		FrameJSON:  []byte(`{"k":"v"}`),
		ExpiresAt:  time.Now().Add(time.Hour),
	}
	if _, err := s.SaveFrame(ctx, wcA(), frame); err != nil {
		t.Fatalf("SaveFrame: %v", err)
	}

	// La lectura ES por proyecto: el filtro del store comprueba contra el
	// proyecto activo (INV-7), asi que el slot se apunta antes de leer.
	s.SetActiveProject(ctx, "acme")
	frames, err := s.ListFrames(ctx, store.FrameListFilters{ProjectID: "acme"})
	if err != nil {
		t.Fatalf("list frames (acme): %v", err)
	}
	if len(frames) == 0 {
		t.Errorf("SaveFrame: no hay frames en acme; el frame se fue al slot global")
	}
	s.SetActiveProject(ctx, "globex")
	framesG, err := s.ListFrames(ctx, store.FrameListFilters{ProjectID: "globex"})
	if err != nil {
		t.Fatalf("list frames (globex): %v", err)
	}
	if len(framesG) != 0 {
		t.Errorf("SaveFrame: %d frame(s) aterrizaron en globex (el slot global); la sesion es de acme", len(framesG))
	}
}
