package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/base64"
	"encoding/json"
 "errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"strconv"
 "sort"
 "loreviva.local/loreviva/internal/memory"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
)

// This fixture uses the existing publication, registrar and execution owners.
// BFF secrets travel only through the owned child's stdin and scan custody.
func referenceWorld(t *testing.T) (integrationFixFixture, string, generated.CreateInstanceRequest, domain.RegistryCommand) {
	t.Helper()
	actions := []string{"READ", "TURN", "CONTROL_PAUSE", "CONTROL_RESUME", "MEMORY_VIEW", "MEMORY_GOVERN"}
	r, _, service, request, reg := integrationCreationFixtureActions(t, nil, time.Hour, actions)
	world, e := r.World.CreateIntegrationInstance(service, "reference-world", request)
	if e != nil {
		t.Fatal("real reference CREATE prerequisite", e)
	}
	f := integrationFixFixture{r: r, service: service, world: world.Scope}
	session, e := r.Store.ExchangeIntegrationSession(service, "reference-fixture-session", "integration_r0_v1", generated.ClientSessionRequest{
		ProductID: "coffee", WorldInstanceID: world.Scope.WorldInstanceID,
		ExternalSubjectID: string(reg.Player.ExternalIDBytes), EntitlementReference: reg.Player.EntitlementID, RequestedActions: actions,
	})
	if e != nil {
		t.Fatal("real reference Session prerequisite", e)
	}
	retainSessionScanMaterial(t, session.AccessToken)
	f.client, _, e = r.Store.AuthenticateIntegration(context.Background(), "Bearer "+session.AccessToken, domain.IntegrationClient)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ApplyPlayerSelfConsent(f.client, "reference-initial", json.RawMessage(`{"command":"INITIAL","expected_revision":"0","policy_id":"R0_PLAYER_PRIVATE_GENERATION_30D"}`)); e != nil {
		t.Fatal(e)
	}
	f.control(t, service, "START", "0", "reference-start")
	var raw []byte
	var item struct{ CustodianID string }
	if e = r.Store.DB.QueryRow("SELECT document FROM items WHERE world_instance_id=? LIMIT 1", f.world.WorldInstanceID).Scan(&raw); e != nil || json.Unmarshal(raw, &item) != nil {
		t.Fatal("real reference actor prerequisite", e)
	}
	f.actor = item.CustodianID
	var wr, cv uint64
	if e = r.Store.DB.QueryRow("SELECT world_revision,control_version FROM world_controls WHERE world_instance_id=?", f.world.WorldInstanceID).Scan(&wr, &cv); e != nil {
		t.Fatal(e)
	}
	f.input = generated.TurnRequest{ExpectedVersions: generated.ExpectedVersions{WorldRevision: strconv.FormatUint(wr,10), ControlVersion: strconv.FormatUint(cv,10)}, PrimaryActorID: f.actor, RequestedAudience: "PRIVATE_RELATIONSHIP", Input: json.RawMessage(`{"kind":"TEXT","text":"May I borrow the sign?"}`)}
	quote, e := r.Store.QuoteIntegrationTurn(service, "reference-quote", generated.QuoteRequest{Scope: f.world, ServiceKind: "TURN_TEXT", PrimaryActorID: &f.actor, InputUpperBound: "16000", OutputUpperBound: "4000", PurchaseResponsibility: "MOCK"})
	if e != nil {
		t.Fatal(e)
	}
	f.input.QuoteID = quote.QuoteID
	accepted, e := r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "reference-intake", f.input)
	if e != nil {
		t.Fatal("real reference TURN prerequisite", e)
	}
	lease, e := r.Store.ClaimRegistered(context.Background(), "reference-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: accepted.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Turns.Execute(context.Background(), lease.TaskLease, "reference-worker"); e != nil {
		t.Fatal("real reference Mock COMMIT prerequisite", e)
	}
	return f, accepted.TurnID, request, reg
}

type referenceBffProcess struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	lines  chan string
	done   chan error
	stderr bytes.Buffer
	finished bool
}

func referenceChildEnv() []string {
	out := []string{}
	for _, value := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(value, "=", 2)[0])
		if strings.Contains(key, "DSN") || strings.Contains(key, "PASSWORD") || strings.Contains(key, "TOKEN") || strings.Contains(key, "SECRET") || strings.HasPrefix(key, "LOREVIVA_") || key == "NODE_OPTIONS" {
			continue
		}
		out = append(out, value)
	}
	return out
}

func referenceStart(t *testing.T, action string, input any) *referenceBffProcess {
	t.Helper()
	node := os.Getenv("LOREVIVA_TEST_NODE_EXE")
	if node == "" {
		t.Fatal("pinned Node runtime required")
	}
	p := &referenceBffProcess{lines: make(chan string, 8), done: make(chan error, 1)}
	p.cmd = exec.Command(node, "examples/thin-backend/reference-bff.ts", action)
	p.cmd.Dir = "../.."
	p.cmd.Env = referenceChildEnv()
	var e error
	p.input, e = p.cmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	output, e := p.cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	p.cmd.Stderr = &p.stderr
	if e = p.cmd.Start(); e != nil {
		t.Fatal("owned BFF child start prerequisite", e)
	}
	// Wait is called only after stdout reaches EOF, so final CLOSED cannot be
	// discarded by Cmd.Wait closing the read pipe early.
	go func() {
		scan := bufio.NewScanner(output)
		scan.Buffer(make([]byte, 1024), 8192)
		for scan.Scan() {
			p.lines <- scan.Text()
		}
		close(p.lines)
		p.done <- p.cmd.Wait()
	}()
	raw, e := json.Marshal(input)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.input.Write(append(raw, '\n')); e != nil {
		t.Fatal("owned startup stdin", e)
	}
	return p
}

func (p *referenceBffProcess) state(t *testing.T, expected string) {
	t.Helper()
	select {
	case line, ok := <-p.lines:
		var v struct{ Format, State, Origin string }
		if !ok || json.Unmarshal([]byte(line), &v) != nil || v.Format != "lv-reference-startup-v1" || v.State != expected {
			t.Fatalf("BFF startup/lifetime %s was not reached", expected)
		}
		if expected == "READY" && v.Origin != "http://127.0.0.1:27185" {
			t.Fatal("BFF loopback identity")
		}
	case <-time.After(25 * time.Second):
		t.Fatal("bounded BFF state timeout", expected)
	}
}

func (p *referenceBffProcess) finish(t *testing.T, stop bool) {
	t.Helper()
	if p.finished {return}
	p.finished=true
	if stop {
		_, _ = io.WriteString(p.input, "STOP\n")
	}
	_ = p.input.Close()
	select {
	case e := <-p.done:
		if e != nil || p.stderr.Len() != 0 {
			// This is the enclosing owned capture's untrusted raw stream. The
			// custodian scans it before any model/user inspection.
			_, _ = os.Stderr.Write(p.stderr.Bytes())
			t.Error("owned BFF child returned an error", e)
		}
	case <-time.After(20 * time.Second):
		_ = p.cmd.Process.Kill() // exact child owned by this fixture, never MySQL
		<-p.done
		t.Error("owned BFF child failed its close budget")
	}
}

func TestReferenceClientBrowserFixture(t *testing.T) {
	f, turnID, creation, registration := referenceWorld(t)
	customerDB := dbfixture.Open(t)
	var name, installation string
	if e := customerDB.QueryRow("SELECT DATABASE()").Scan(&name); e != nil {
		t.Fatal(e)
	}
	if e := f.r.Store.DB.QueryRow("SELECT installation_id FROM memory_installation WHERE singleton=1").Scan(&installation); e != nil {
		t.Fatal(e)
	}
	dsn, e := driver.ParseDSN(os.Getenv("LOREVIVA_TEST_DSN"))
	if e != nil {
		t.Fatal("customer connector prerequisite")
	}
	dsn.DBName, dsn.ParseTime = name, true
	retainSessionScanMaterial(t, dsn.FormatDSN())
	var mu sync.Mutex
	var materials []string
	var receiptPosts atomic.Int32
	var premature atomic.Bool
	type actualWrite struct { method, target, profile, key string; body []byte }
	var sent []actualWrite
	var droppedControls []string
	serviceTasks:=map[string]bool{};taskReads:=map[string]int{}
	collect := func(raw []byte) {
		var p generated.IntegrationNotificationPage
		if generated.Decode("IntegrationNotificationPage", raw, &p) == nil {
			mu.Lock()
			defer mu.Unlock()
			materials = append(materials, p.NextCursor)
			for _, n := range p.Notifications {
				materials = append(materials, n.Challenge.Nonce)
			}
		}
		var session generated.ClientSession
		if generated.Decode("ClientSession", raw, &session) == nil {
			mu.Lock()
			materials = append(materials, session.AccessToken)
			mu.Unlock()
		}
	}
	defer func() {
		for _, value := range materials {
			if value != "" {
				retainSessionScanMaterial(t, value)
			}
		}
	}()
	handler := f.r.Handler()
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dropControl := ""
		if r.Method==http.MethodGet && strings.HasPrefix(r.URL.Path,"/world/v1/tasks/"){mu.Lock();taskReads[strings.TrimPrefix(r.URL.Path,"/world/v1/tasks/")]++;mu.Unlock()}
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Loreviva-Csrf") != "" {
			premature.Store(true)
			w.WriteHeader(400)
			return
		}
		if r.Method == http.MethodPost {
			raw, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
			if err != nil || len(raw)>1<<20 { w.WriteHeader(400); return }
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(raw))
			mu.Lock()
			sent=append(sent,actualWrite{r.Method,r.URL.RequestURI(),r.Header.Get("Loreviva-Client-Contract"),r.Header.Get("Idempotency-Key"),append([]byte(nil),raw...)})
			mu.Unlock()
			if strings.HasSuffix(r.URL.Path,"/controls"){
				var body struct{Command string};if json.Unmarshal(raw,&body)==nil&&(body.Command=="START"||body.Command=="END"){dropControl=body.Command}
			}
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/receipts") {
			parts := strings.Split(r.URL.Path, "/")
			var count int
			e := customerDB.QueryRow("SELECT COUNT(*) FROM customer_inbox i JOIN customer_messages m USING(installation_id,customer_id,consumer_id,notification_ref,initial_revision) WHERE i.notification_ref=? AND i.status='COMMITTED' AND i.message_id=m.message_id", parts[len(parts)-2]).Scan(&count)
			if e != nil || count != 1 {
				premature.Store(true)
				w.WriteHeader(500)
				return
			}
			receiptPosts.Add(1)
		}
		if dropControl!=""{
			// The real handler completes its actual command/COMMIT first. Only
			// its successful response is lost; no task or receipt is fabricated.
			out:=httptest.NewRecorder();handler.ServeHTTP(out,r)
			if out.Code==http.StatusAccepted{
				var receipt generated.TaskReceipt
				if generated.Decode("TaskReceipt",out.Body.Bytes(),&receipt)!=nil{premature.Store(true);w.WriteHeader(500);return}
				mu.Lock();droppedControls=append(droppedControls,dropControl);serviceTasks[string(receipt.TaskID)]=true;mu.Unlock()
				if h,ok:=w.(http.Hijacker);ok{conn,_,err:=h.Hijack();if err==nil{_ = conn.Close();return}}
				premature.Store(true);return
			}
			for key,values:=range out.Header(){for _,value:=range values{w.Header().Add(key,value)}}
			w.WriteHeader(out.Code);_,_=w.Write(out.Body.Bytes());return
		}
		handler.ServeHTTP(&customerScanWriter{ResponseWriter: w, collect: collect}, r)
	}))
	defer platform.Close()
	exe := buildCustomerExample(t)
	config := map[string]string{"DSN": dsn.FormatDSN(), "BaseURL": platform.URL, "ServiceToken": "synthetic-s66-create-service-secret-32", "Installation": installation, "Customer": "s66-customer", "World": f.world.WorldInstanceID}
	customer := &customerHarness{db: customerDB, exe: exe, cfg: config}
	initialized, e := customer.run(t, "init", "")
	if e != nil || initialized.State != "INITIALIZED" {
		t.Fatal("original two-table initializer prerequisite", e, initialized.State)
	}
	body, e := os.ReadFile(exe)
	if e != nil {
		t.Fatal(e)
	}
	hash := sha256.Sum256(body)
	journalRoot := filepath.Join(t.TempDir(), "journal")
	journal := map[string]any{"root": journalRoot, "installation": installation, "windows": map[string]any{
		"executable": "C:/Windows/System32/WindowsPowerShell/v1.0/powershell.exe", "bytes": 495616,
		"sha256": "60c9b29843624dd8af6b6a7b26147753be6aeef65098b9a503cc50850afe410f", "volume": "1153549677971061842", "fileId": "281474978578202", "userSID": "S-1-5-21-3980322072-4285430573-1333711809-1003",
	}}
	init := referenceStart(t, "initialize", journal)
	defer init.finish(t, false)
	init.state(t, "INITIALIZED")
	init.finish(t, false)
	keyBody, e := os.ReadFile(filepath.Join(journalRoot, "key.json"))
	var key struct{ KeyID, KeyBase64 string }
	var keyObject map[string]string
	if e != nil || json.Unmarshal(keyBody, &keyObject) != nil {
		t.Fatal("actual journal key custody", e)
	}
	key.KeyID, key.KeyBase64 = keyObject["key_id"], keyObject["key_base64"]
	retainSessionScanMaterial(t, key.KeyID)
	retainSessionScanMaterial(t, key.KeyBase64)
	keyRaw,e:=base64.StdEncoding.DecodeString(key.KeyBase64)
	if e!=nil||len(keyRaw)!=32 {t.Fatal("actual generated journal key shape")}
	retainSessionScanMaterial(t,hex.EncodeToString(keyRaw))
	credential := "synthetic-reference-login-credential"
	retainSessionScanMaterial(t, credential)
	credentialHash := sha256.Sum256([]byte(credential))
	options := map[string]any{"platformOrigin": platform.URL, "port": 27185, "journal": journal,
		"users": []any{map[string]any{"login": "reference", "credentialSHA256": hex.EncodeToString(credentialHash[:]), "tenant": "demo-local", "product": "coffee", "customer": "s66-customer", "serviceSubject": "s66-service", "player": "s66-player", "externalSubject": "S66-player-external", "entitlement": "s66-entitlement-one", "world": f.world.WorldInstanceID, "profile": "integration_r0_v1", "requestedActions": []string{"READ", "TURN", "CONTROL_PAUSE", "CONTROL_RESUME", "MEMORY_VIEW", "MEMORY_GOVERN"}, "serviceKey": "synthetic-s66-create-service-secret-32"}},
		"customer": map[string]any{"executable": map[string]any{"path": exe, "bytes": len(body), "sha256": hex.EncodeToString(hash[:])}, "databases": []any{map[string]any{"customer": "s66-customer", "world": f.world.WorldInstanceID, "dsn": dsn.FormatDSN()}}},
	}

 // A second real registered person starts without positive use or world START.
 // The browser performs these commands; no SQL grants or synthetic receipt.
 next:=*registration.Player
 next.PlayerSubjectID="reference-player-two"
 next.ExternalIDBytes=[]byte("reference-external-two")
 next.EntitlementID="reference-entitlement-two"
 nextCommand:=domain.RegistryCommand{Kind:"REGISTER_PLAYER_ACCESS",ProductID:"coffee",TargetID:next.PlayerSubjectID,Player:&next}
 operator:=runtimeContext(t,f.r)
 if _,e=f.r.Store.ApplyIntegrationRegistry(operator,"reference-register-two",nextCommand);e!=nil{t.Fatal("real second person registration",e)}
 freshRequest:=creation;freshRequest.InitialParticipants=[]string{next.PlayerSubjectID}
 freshWorld,e:=f.r.World.CreateIntegrationInstance(f.service,"reference-fresh-world",freshRequest)
 if e!=nil{t.Fatal("real fresh browser world",e)}
 // Separate unbound entitlement permits the browser's explicit new CREATE.
 extra:=next;extra.EntitlementID="reference-entitlement-ui-create"
 nextCommand.Player=&extra
 if _,e=f.r.Store.ApplyIntegrationRegistry(operator,"reference-create-entitlement",nextCommand);e!=nil{t.Fatal(e)}
 var aggregateRaw []byte
 if e=f.r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id=?",freshWorld.Scope.WorldInstanceID,next.PlayerSubjectID).Scan(&aggregateRaw);e!=nil{t.Fatal(e)}
 var aggregate memory.Aggregate
 if e=json.Unmarshal(aggregateRaw,&aggregate);e!=nil{t.Fatal(e)}
 ids:=[]string{};for id,node:=range aggregate.Nodes{if node.Domain!=memory.WorldFact{ids=append(ids,id)}};sort.Strings(ids)
 if len(ids)==0{t.Fatal("real authored non-world-fact memory prerequisite")}
 var freshActor string
 var freshItemRaw []byte;var freshItem struct{CustodianID string}
 if e=f.r.Store.DB.QueryRow("SELECT document FROM items WHERE world_instance_id=? ORDER BY item_id LIMIT 1",freshWorld.Scope.WorldInstanceID).Scan(&freshItemRaw);e!=nil{t.Fatal(e)}
 if e=json.Unmarshal(freshItemRaw,&freshItem);e!=nil{t.Fatal(e)};freshActor=freshItem.CustodianID
 fresh:=map[string]string{"login":"reference-two","player":next.PlayerSubjectID,"world":freshWorld.Scope.WorldInstanceID,"release":creation.ReleaseID,"actor":freshActor,"memory":ids[0]}

 users:=options["users"].([]any);second:=map[string]any{}
 for k,v:=range users[0].(map[string]any){second[k]=v}
 second["login"],second["player"],second["externalSubject"],second["entitlement"],second["world"]="reference-two",next.PlayerSubjectID,string(next.ExternalIDBytes),next.EntitlementID,freshWorld.Scope.WorldInstanceID
 options["users"]=append(users,second)
 databases:=options["customer"].(map[string]any)["databases"].([]any)
 options["customer"].(map[string]any)["databases"]=append(databases,map[string]any{"customer":"s66-customer","world":freshWorld.Scope.WorldInstanceID,"dsn":dsn.FormatDSN()})
	child := referenceStart(t, "serve", options)
	defer child.finish(t, true)
	child.state(t, "READY")
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 65 * time.Second}
	csrf := ""
	post := func(path string, input any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(input)
		req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:27185"+path, bytes.NewReader(raw))
		req.Header.Set("Origin", "http://127.0.0.1:27185")
		req.Header.Set("Content-Type", "application/json")
		if csrf != "" {
			req.Header.Set("X-Loreviva-Csrf", csrf)
		}
		resp, e := client.Do(req)
		if e != nil {
			t.Fatal("real BFF request transport", e)
		}
		defer resp.Body.Close()
		raw, e = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var value map[string]any
		if e != nil || json.Unmarshal(raw, &value) != nil {
			t.Fatal("real BFF JSON response", e)
		}
		if v, ok := value["csrf"].(string); ok {
			retainSessionScanMaterial(t, v)
			csrf = v
		}
		for _, cookie := range resp.Cookies() {
			if cookie.Value != "" {
				retainSessionScanMaterial(t, cookie.Value)
			}
		}
		if resp.StatusCode != 200 {
			t.Fatalf("real BFF %s status %d code %v", path, resp.StatusCode, value["code"])
		}
		return value
	}
	post("/bff/login", map[string]any{"format": "lv-bff-login-v1", "login": "reference", "credential": credential})
	post("/bff/exchange", map[string]any{"format": "lv-bff-exchange-v1", "original_key": "reference-bff-exchange"})
	result := post("/bff/customer-consume", map[string]any{"format": "lv-bff-consume-v1"})
	if result["format"] != "lv-bff-customer-result-v1" || result["State"] != "CONFIRMED" || result["Persisted"] != float64(2) || result["Confirmed"] != float64(2) || receiptPosts.Load() != 2 || premature.Load() {
		t.Fatal("real original customer result/commit-before-ACK", result["State"], receiptPosts.Load(), premature.Load())
	}
	var messages, inbox, acked int
	if e = customerDB.QueryRow("SELECT (SELECT COUNT(*) FROM customer_messages),(SELECT COUNT(*) FROM customer_inbox WHERE status='COMMITTED')").Scan(&messages, &inbox); e != nil || messages != 2 || inbox != 2 {
		t.Fatal("actual separate customer records", messages, inbox, e)
	}
	if e = f.r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_notifications n JOIN outbox o ON o.tenant_id=n.tenant_id AND o.product_id=n.product_id AND o.record_id=n.outbox_ref WHERE o.status='ACKED' AND n.world_instance_id=?", f.world.WorldInstanceID).Scan(&acked); e != nil || acked != 2 {
		t.Fatal("actual platform ACK records", acked, e)
	}
	t.Log(fmt.Sprintf("real platform TURN %s, two customer records and two platform ACKs established before browser", turnID))
 workerContext,workerCancel:=context.WithCancel(context.Background());workerDone:=make(chan error,1)
 go func(){ticker:=time.NewTicker(200*time.Millisecond);defer ticker.Stop();for{select{case<-workerContext.Done():workerDone<-nil;return;case<-ticker.C:if err:=f.r.Worker.Once(workerContext);err!=nil&&!errors.Is(err,store.ErrNoTask){if workerContext.Err()!=nil{workerDone<-nil}else{workerDone<-err};return}}}}()
 mediaContext,mediaCancel:=context.WithCancel(context.Background());mediaDone:=make(chan error,1)
 go func(){ticker:=time.NewTicker(200*time.Millisecond);defer ticker.Stop();for{select{case<-mediaContext.Done():mediaDone<-nil;return;case<-ticker.C:if _,err:=f.r.Delivery.ProduceOneSyntheticMedia(mediaContext);err!=nil{if mediaContext.Err()!=nil{mediaDone<-nil}else{mediaDone<-err};return}}}}()
 defer func(){mediaCancel();workerCancel();if err:=<-mediaDone;err!=nil{t.Error("owned actual media producer",err)};select{case err:=<-workerDone:if err!=nil{t.Error("owned actual Worker.Once",err)};case<-time.After(10*time.Second):t.Error("owned Worker.Once close timeout")}}()

	browserInput, _ := json.Marshal(map[string]any{"origin": "http://127.0.0.1:27185", "login": "reference", "credential": credential, "player": "s66-player", "world": f.world.WorldInstanceID, "turn": turnID,"fresh":fresh})
	browserContext, browserCancel := context.WithTimeout(context.Background(), 190*time.Second)
	defer browserCancel()
	browser := exec.CommandContext(browserContext, os.Getenv("LOREVIVA_TEST_NODE_EXE"), "scripts/reference-browser.mjs")
	browser.Dir = "../.."
	browser.Env = append(referenceChildEnv(), "LOREVIVA_TEST_CUSTODY_FILE="+os.Getenv("LOREVIVA_TEST_CUSTODY_FILE"))
	browser.Stdin = bytes.NewReader(browserInput)
	var browserOut, browserError bytes.Buffer
	browser.Stdout, browser.Stderr = &browserOut, &browserError
	browserResult := browser.Run()
	_, _ = os.Stdout.Write(browserOut.Bytes())
	_, _ = os.Stderr.Write(browserError.Bytes())
	if browserResult != nil {
		t.Fatal("actual reference-player-real-loopback did not complete", browserResult)
	}
	if e = customerDB.QueryRow("SELECT (SELECT COUNT(*) FROM customer_messages),(SELECT COUNT(*) FROM customer_inbox WHERE status='COMMITTED')").Scan(&messages, &inbox); e != nil || messages != 6 || inbox != 6 {
		t.Fatal("browser customer original commits/cardinality", messages, inbox, e)
	}
	if premature.Load() || receiptPosts.Load()!=6 {t.Fatal("browser actual customer commit precedes each platform ACK")}
	mu.Lock()
	if len(droppedControls)!=2||droppedControls[0]!="START"||droppedControls[1]!="END"{mu.Unlock();t.Fatal("real successful CONTROL responses were not both lost")}
	for task:=range serviceTasks{if taskReads[task]!=0{mu.Unlock();t.Fatal("SERVICE original task queried with browser CLIENT")}}
	mu.Unlock()
	child.finish(t,true)
	// This is the owned synthetic journal after the real writer has closed.
	// Decryption is in RAM only; assertions never print keys or request bodies.
	journalBytes,e:=os.ReadFile(filepath.Join(journalRoot,"journal.bin"))
	if e!=nil||!bytes.HasPrefix(journalBytes,[]byte("LVJ1\n")){t.Fatal("closed journal header")}
	block,e:=aes.NewCipher(keyRaw);if e!=nil{t.Fatal("owned key")}
	aead,e:=cipher.NewGCM(block);if e!=nil{t.Fatal("owned AEAD")}
	offset,seq,previous:=5,1,strings.Repeat("0",64)
	operations:=map[string]int{}
	mu.Lock();observed:=append([]actualWrite(nil),sent...);mu.Unlock()
	for offset<len(journalBytes){
		if len(journalBytes)-offset<32{t.Fatal("journal frame truncated")}
		length:=int(binary.BigEndian.Uint32(journalBytes[offset:offset+4]))
		end:=offset+4+12+length+16
		if length<1||length>1<<20||end>len(journalBytes){t.Fatal("journal frame bound")}
		frame:=journalBytes[offset:end]
		aad,_:=json.Marshal([]string{"lv-reference-journal-v1",installation,key.KeyID,strconv.Itoa(seq),previous})
		plain,err:=aead.Open(nil,frame[4:16],frame[16:],aad);if err!=nil{t.Fatal("journal actual authenticated frame")}
		var row struct{ Kind,Operation string; Seq string; OriginalKey string `json:"original_key"`; Target []string `json:"request_target"`; Request string `json:"request_base64"`; Hash string `json:"request_sha256"` }
		if json.Unmarshal(plain,&row)!=nil||row.Seq!=strconv.Itoa(seq){t.Fatal("journal sequence")}
		if row.Kind=="ORIGINAL"{
			if len(row.Target)!=3{t.Fatal("journal target")}
			raw,err:=base64.StdEncoding.DecodeString(row.Request);if err!=nil{t.Fatal("journal raw encoding")}
			h:=sha256.Sum256(raw);if hex.EncodeToString(h[:])!=row.Hash{t.Fatal("journal exact request hash")}
			matches:=0
			for _,request:=range observed{if request.key==row.OriginalKey&&request.target==row.Target[1]{
				if request.method!=row.Target[0]||request.profile!=row.Target[2]||!bytes.Equal(request.body,raw){t.Fatal("real platform bytes differ from original journal")}
				matches++
			}}
			if matches!=1{t.Fatalf("original %s platform send count=%d",row.Operation,matches)}
			operations[row.Operation]++
		}
		h:=sha256.Sum256(frame);previous=hex.EncodeToString(h[:]);offset=end;seq++
	}
	for _,operation:=range []string{"createInstance","controlInstance","createQuote","submitTurn","commandPlayerSelfConsent","governMemories","cancelTask"}{
		if operations[operation]==0{t.Fatal("real browser broker operation missing",operation)}
	}
	t.Log("Seven real browser broker operations match closed authenticated journal bytes; original recovery adds zero business re-send; customer messages=6 and ACKs=6.")
}
