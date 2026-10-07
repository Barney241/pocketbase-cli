package cli_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"mime/quotedprintable"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barney241/pocketbase-cli/internal/cli"
)

type mailbox struct {
	mutex    sync.Mutex
	messages []string
	address  string
}

func startMailbox(t *testing.T) *mailbox {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	box := &mailbox{address: listener.Addr().String()}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go box.receive(connection)
		}
	}()
	return box
}

func (m *mailbox) receive(connection net.Conn) {
	defer connection.Close()
	reader := bufio.NewReader(connection)
	reply := func(line string) { io.WriteString(connection, line+"\r\n") }
	reply("220 mailbox ready")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
			reply("250 mailbox")
		case command == "DATA":
			reply("354 go ahead")
			m.store(readMessage(reader))
			reply("250 stored")
		case command == "QUIT":
			reply("221 bye")
			return
		default:
			reply("250 ok")
		}
	}
}

func readMessage(reader *bufio.Reader) string {
	message := &strings.Builder{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil || line == ".\r\n" {
			return message.String()
		}
		message.WriteString(line)
	}
}

func (m *mailbox) store(message string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.messages = append(m.messages, message)
}

func (m *mailbox) count() int {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return len(m.messages)
}

func (m *mailbox) next(t *testing.T, alreadyRead int) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		m.mutex.Lock()
		if len(m.messages) > alreadyRead {
			message := m.messages[alreadyRead]
			m.mutex.Unlock()
			return decodeMailBody(message)
		}
		m.mutex.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no email arrived (have %d)", alreadyRead)
	return ""
}

func decodeMailBody(message string) string {
	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(message)))
	if err != nil {
		return message
	}
	return string(decoded)
}

var (
	mailedToken = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
	mailedCode  = regexp.MustCompile(`>\s*(\d{4,12})\s*<`)
)

func tokenFrom(t *testing.T, mail string) string {
	t.Helper()
	token := mailedToken.FindString(mail)
	if token == "" {
		t.Fatalf("no token in the email:\n%s", mail)
	}
	return token
}

func (m *mailbox) deliverThrough(t *testing.T) {
	t.Helper()
	host, port, _ := net.SplitHostPort(m.address)
	mustRun(t, "settings", "update", "smtp.enabled:=true", "smtp.host="+host, "smtp.port:="+port, "smtp.tls:=false", "meta.senderAddress=pb@test.dev", "--yes")
}

func jsonField(t *testing.T, document, field string) string {
	t.Helper()
	object := map[string]any{}
	if err := json.Unmarshal([]byte(document), &object); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, document)
	}
	value, _ := object[field].(string)
	if value == "" {
		t.Fatalf("no %q in %s", field, document)
	}
	return value
}

func TestEveryAuthFlowWorksEndToEndThroughRealEmails(t *testing.T) {
	startPocketBase(t, guardedByHook)
	seedPosts(t)
	box := startMailbox(t)
	box.deliverThrough(t)
	mustRun(t, "collections", "update", "members", "otp.enabled:=true", "--yes")

	mustRun(t, "settings", "test-email", "--to", "probe@test.dev", "--template", "verification", "--collection", "members")
	if probe := box.next(t, 0); !strings.Contains(probe, "probe@test.dev") {
		t.Fatalf("the test email went elsewhere:\n%s", probe)
	}

	memberID := strings.TrimSpace(mustRun(t, "records", "create", "members", "email=ann@test.dev", "password="+testPassword, "passwordConfirm="+testPassword, "-q"))
	if methods := mustRun(t, "auth", "methods", "members"); !strings.Contains(methods, `"password"`) || !strings.Contains(methods, `"otp"`) {
		t.Fatalf("unexpected auth methods: %s", methods)
	}

	read := box.count()
	mustRun(t, "auth", "request-verification", "members", "email=ann@test.dev")
	mustRun(t, "auth", "confirm-verification", "members", "token="+tokenFrom(t, box.next(t, read)))
	if verified := mustRun(t, "records", "get", "members", memberID, "-o", "json"); !strings.Contains(verified, `"verified":true`) {
		t.Fatalf("the member is not verified: %s", verified)
	}

	read = box.count()
	otpID := jsonField(t, mustRun(t, "auth", "request-otp", "members", "email=ann@test.dev"), "otpId")
	code := mailedCode.FindStringSubmatch(box.next(t, read))
	if code == nil {
		t.Fatal("no one-time password in the email")
	}
	if session := mustRun(t, "auth", "with-otp", "members", "otpId="+otpID, "password="+code[1]); !strings.Contains(session, `"token"`) {
		t.Fatalf("OTP login returned %s", session)
	}

	const newPassword = "Changed5678!pbctl"
	read = box.count()
	mustRun(t, "auth", "request-password-reset", "members", "email=ann@test.dev")
	mustRun(t, "auth", "confirm-password-reset", "members", "token="+tokenFrom(t, box.next(t, read)), "password="+newPassword, "passwordConfirm="+newPassword)
	if refused := run(t, "", "auth", "login", "members", "identity=ann@test.dev", "password="+testPassword); refused.code == 0 {
		t.Fatal("the old password still works after a reset")
	}
	if session := mustRun(t, "auth", "login", "members", "identity=ann@test.dev", "password="+newPassword); !strings.Contains(session, memberID) {
		t.Fatalf("login with the new password returned %s", session)
	}

	read = box.count()
	mustRun(t, "--as", "members/"+memberID, "auth", "request-email-change", "members", "newEmail=ann.new@test.dev")
	mustRun(t, "auth", "confirm-email-change", "members", "token="+tokenFrom(t, box.next(t, read)), "password="+newPassword)
	if renamed := mustRun(t, "records", "get", "members", memberID, "--fields", "email", "-o", "json"); !strings.Contains(renamed, "ann.new@test.dev") {
		t.Fatalf("the email did not change: %s", renamed)
	}

	impersonation := mustRun(t, "auth", "impersonate", "members", memberID, "duration:=120")
	if jsonField(t, impersonation, "token") == "" {
		t.Fatal("impersonation returned no token")
	}
	if refreshed := mustRun(t, "auth", "refresh", "_superusers"); !strings.Contains(refreshed, adminEmail) {
		t.Fatalf("refresh returned %s", refreshed)
	}
}

func TestBackupsCanBeCreatedMovedAndRestored(t *testing.T) {
	pocketBase := startPocketBase(t, guardedByHook)
	seedPosts(t)
	mustRun(t, "backups", "create", "before.zip")
	if listed := mustRun(t, "backups", "list"); !strings.Contains(listed, "before.zip") {
		t.Fatalf("the backup is not listed:\n%s", listed)
	}
	archive := filepath.Join(t.TempDir(), "copy.zip")
	saved := mustRun(t, "backups", "download", "before.zip", "--to", archive)
	if info, err := os.Stat(archive); err != nil || info.Size() < 1000 || !strings.Contains(saved, archive) {
		t.Fatalf("download wrote %v (%v) and printed %q", info, err, saved)
	}
	mustRun(t, "backups", "delete", "before.zip", "--yes")
	if listed := run(t, "", "backups", "list"); strings.Contains(listed.stdout, "before.zip") {
		t.Fatalf("the backup survived a delete:\n%s", listed.stdout)
	}
	mustRun(t, "backups", "upload", archive)
	if listed := mustRun(t, "backups", "list"); !strings.Contains(listed, "copy.zip") {
		t.Fatalf("the uploaded backup is not listed:\n%s", listed)
	}

	mustRun(t, "records", "create", "posts", "title=made after the backup")
	if count := strings.TrimSpace(mustRun(t, "records", "count", "posts")); count != "3" {
		t.Fatalf("count before the restore = %s, want 3", count)
	}
	mustRun(t, "backups", "restore", "copy.zip", "--yes")
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if counted := run(t, "", "records", "count", "posts"); counted.code == 0 && strings.TrimSpace(counted.stdout) == "2" {
			pocketBase.waitUntilHealthy()
			return
		}
	}
	t.Fatalf("the restore never brought the data back: %+v", run(t, "", "records", "count", "posts"))
}

func startBarePocketBase(t *testing.T) string {
	t.Helper()
	binary := os.Getenv(envTestServer)
	dataDir := filepath.Join(t.TempDir(), "pb_data")
	if out, err := exec.Command(binary, "superuser", "upsert", adminEmail, testPassword, "--dir", dataDir).CombinedOutput(); err != nil {
		t.Fatalf("create superuser: %v\n%s", err, out)
	}
	address := freeAddress(t)
	server := exec.Command(binary, "serve", "--http", address, "--dir", dataDir, "--dev=false")
	server.Env = append(os.Environ(), "PBSERVER_HOOKS_DIR="+filepath.Join(t.TempDir(), "no_hooks"))
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		server.Process.Kill()
		server.Wait()
	})
	second := &instance{t: t, url: "http://" + address}
	second.waitUntilHealthy()
	return second.url
}

func TestASchemaMovesBetweenInstancesAndTheDiffFollows(t *testing.T) {
	pocketBase := startPocketBase(t, guardedByHook)
	seedPosts(t)
	pocketBase.addProfile("other", map[string]any{"url": startBarePocketBase(t), "identity": adminEmail, "password": testPassword, "read_only": false})

	if before := mustRun(t, "collections", "diff", "other"); !strings.Contains(before, "+ posts (base)") || !strings.Contains(before, "+ members (auth)") {
		t.Fatalf("the diff misses the new collections:\n%s", before)
	}
	exported := mustRun(t, "collections", "export", "posts", "members")
	exportFile := filepath.Join(t.TempDir(), "schema.json")
	os.WriteFile(exportFile, []byte(exported), 0o600)
	mustRun(t, "-p", "other", "collections", "import", exportFile, "--yes")
	if same := run(t, "", "collections", "diff", "other", "posts", "members"); same.code != 0 || !strings.Contains(same.stderr, "schemas match") {
		t.Fatalf("the imported schema differs:\n%s%s", same.stdout, same.stderr)
	}

	mustRun(t, "-p", "other", "collections", "update", "posts", "listRule=", "--yes")
	if drifted := mustRun(t, "collections", "diff", "other", "posts"); !strings.Contains(drifted, "~ posts") || !strings.Contains(drifted, "listRule") {
		t.Fatalf("the diff misses the changed rule:\n%s", drifted)
	}

	mustRun(t, "-p", "other", "records", "create", "posts", "title=to be truncated")
	mustRun(t, "-p", "other", "collections", "truncate", "posts", "--yes")
	if count := strings.TrimSpace(mustRun(t, "-p", "other", "records", "count", "posts")); count != "0" {
		t.Fatalf("count after truncate = %s", count)
	}
	mustRun(t, "-p", "other", "collections", "delete", "posts", "--yes")
	if gone := run(t, "", "-p", "other", "collections", "show", "posts"); gone.code != 5 {
		t.Fatalf("show of a deleted collection exited %d, want 5", gone.code)
	}
}

func TestTheRemainingCommandsWorkAgainstARealServer(t *testing.T) {
	pocketBase := startPocketBase(t, guardedByHook)
	firstID := seedPosts(t)

	if guide := mustRun(t, "guide"); !strings.Contains(guide, "Filter syntax") {
		t.Fatal("the guide is empty")
	}

	watched := make(chan result, 1)
	go func() { watched <- run(t, "", "records", "watch", "posts", "--max", "1", "--duration", "20s") }()
	time.Sleep(time.Second)
	mustRun(t, "records", "create", "posts", "title=seen live")
	if event := <-watched; event.code != 0 || !strings.Contains(event.stdout, `"action":"create"`) || !strings.Contains(event.stdout, "seen live") {
		t.Fatalf("watch printed %q %q", event.stdout, event.stderr)
	}

	batch := `[{"method":"POST","url":"/api/collections/posts/records","body":{"title":"from a batch"}},
		{"method":"PATCH","url":"/api/collections/posts/records/` + firstID + `","body":{"views":99}}]`
	if outcome := mustRun(t, "records", "batch", "-d", batch, "--yes"); !strings.Contains(outcome, "from a batch") {
		t.Fatalf("batch returned %s", outcome)
	}
	upsert := `{"id":"` + firstID + `","title":"replaced by an upsert","views":5}`
	if imported := run(t, upsert, "records", "import", "posts", "-", "--upsert", "--yes"); imported.code != 0 {
		t.Fatalf("upsert import failed: %s", imported.stderr)
	}
	if replaced := mustRun(t, "records", "get", "posts", firstID, "--fields", "title,views", "-o", "json"); !strings.Contains(replaced, "replaced by an upsert") {
		t.Fatalf("the upsert did not replace the record: %s", replaced)
	}

	if selected := mustRun(t, "sql", "SELECT title, views FROM posts ORDER BY views DESC LIMIT 1"); !strings.Contains(selected, "title\tviews") || !strings.Contains(selected, "replaced by an upsert\t5") {
		t.Fatalf("sql select printed:\n%s", selected)
	}
	if unconfirmed := run(t, "", "sql", "UPDATE posts SET views = 1"); unconfirmed.code != 6 {
		t.Fatalf("a writing query without --yes exited %d, want 6", unconfirmed.code)
	}
	mustRun(t, "sql", "UPDATE posts SET views = 1", "--yes")
	if count := strings.TrimSpace(mustRun(t, "records", "count", "posts", "-f", "views = 1")); count != "4" {
		t.Fatalf("the sql update touched %s rows, want 4", count)
	}

	crons := mustRun(t, "crons", "list")
	if !strings.Contains(crons, "__pbLogsCleanup__") {
		t.Fatalf("unexpected crons:\n%s", crons)
	}
	mustRun(t, "crons", "run", "__pbLogsCleanup__")

	if failed := run(t, "", "settings", "test-s3"); failed.code != 1 || !strings.Contains(failed.stderr, "400") {
		t.Fatalf("test-s3 without S3 exited %d: %s", failed.code, failed.stderr)
	}
	secret := mustRun(t, "settings", "apple-client-secret", "clientId=dev.test.app", "teamId=ABCDE12345", "keyId=KEYID12345", "privateKey="+freshApplePrivateKey(t), "duration:=3600")
	if jsonField(t, secret, "secret") == "" {
		t.Fatal("no Apple client secret")
	}

	attachment := filepath.Join(t.TempDir(), "plan.txt")
	os.WriteFile(attachment, []byte("plan"), 0o600)
	withFile := strings.TrimSpace(mustRun(t, "records", "create", "posts", "title=has a file", "--file", "doc="+attachment, "-q"))
	filename := strings.Fields(strings.Split(strings.TrimSpace(mustRun(t, "files", "list", "posts", withFile)), "\n")[1])[1]
	plainURL := strings.TrimSpace(mustRun(t, "files", "url", "posts", withFile, filename, "--thumb", "10x10"))
	if !strings.HasPrefix(plainURL, pocketBase.url+"/api/files/posts/"+withFile+"/") || !strings.Contains(plainURL, "thumb=10x10") {
		t.Fatalf("unexpected file url %s", plainURL)
	}
	mustRun(t, "records", "update", "posts", withFile, "doc-="+filename)
	if files := run(t, "", "files", "list", "posts", withFile); !strings.Contains(files.stderr, "no files") {
		t.Fatalf("the file was not removed: %s %s", files.stdout, files.stderr)
	}

	if patched := mustRun(t, "api", "PATCH", "/api/collections/posts/records/"+firstID, "title=patched over api"); !strings.Contains(patched, "patched over api") {
		t.Fatalf("api PATCH returned %s", patched)
	}
	if health := mustRun(t, "api", "GET", "/api/health", "--anonymous"); !strings.Contains(health, "API is healthy") {
		t.Fatalf("api GET returned %s", health)
	}
	if guest := run(t, "", "api", "GET", "/api/collections", "--anonymous"); guest.code != exitAuth {
		t.Fatalf("an anonymous call to a superuser route exited %d, want %d: %s", guest.code, exitAuth, guest.stdout)
	}

	entryID := firstLogID(t)
	if entry := mustRun(t, "logs", "get", entryID); !strings.Contains(entry, "id: "+entryID) {
		t.Fatalf("logs get printed:\n%s", entry)
	}
	if stats := mustRun(t, "logs", "stats", "--since", "1h"); !strings.Contains(stats, "date\ttotal") {
		t.Fatalf("logs stats printed:\n%s", stats)
	}
	mustRun(t, "logs", "truncate", "--yes")
	if remaining := run(t, "", "logs", "list", "-f", "id = '"+entryID+"'"); remaining.code != 0 || strings.Contains(remaining.stdout, entryID) {
		t.Fatalf("a log entry survived truncate:\n%s", remaining.stdout)
	}
}

func freshApplePrivateKey(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
}

func firstLogID(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		listed := strings.Split(strings.TrimSpace(mustRun(t, "logs", "list", "-n", "1")), "\n")
		if len(listed) == 2 {
			return strings.Fields(listed[1])[0]
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("no log entry was ever written")
	return ""
}

func TestProfilesAreManagedFromTheCommandLineAndStayReadOnlyWithoutAPerson(t *testing.T) {
	pocketBase := startPocketBase(t, guardedByHook)

	added := run(t, testPassword+"\n", "profile", "add", "fresh", "--url", pocketBase.url+"/", "--identity", adminEmail, "--password-stdin", "--use")
	if added.code != 0 || !strings.Contains(added.stdout, "added fresh (read-only)") {
		t.Fatalf("profile add: %+v", added)
	}
	if listed := mustRun(t, "profile", "list"); !strings.Contains(listed, "fresh\ttrue\t"+pocketBase.url+"\t"+adminEmail+"\tread-only") || strings.Contains(listed, testPassword) {
		t.Fatalf("unexpected profile list:\n%s", listed)
	}
	if status := mustRun(t, "status"); !strings.Contains(status, "profile: fresh") || !strings.Contains(status, "mode: read-only") {
		t.Fatalf("unexpected status:\n%s", status)
	}
	for _, humanOnly := range [][]string{
		{"profile", "unlock", "fresh"},
		{"profile", "add", "loose", "--url", pocketBase.url, "--writable"},
	} {
		if outcome := run(t, "fresh\nloose\n", humanOnly...); outcome.code != 6 {
			t.Errorf("pbctl %s exited %d without a terminal, want 6", strings.Join(humanOnly, " "), outcome.code)
		}
	}
	if blocked := run(t, "", "collections", "create", "-d", `{"name":"x","type":"base"}`); blocked.code != exitBlocked {
		t.Fatalf("a profile added from the command line can write: exit %d", blocked.code)
	}
	mustRun(t, "profile", "use", "admin")
	mustRun(t, "profile", "lock", "admin")
	if blocked := run(t, "", "collections", "create", "-d", `{"name":"x","type":"base"}`); blocked.code != exitBlocked {
		t.Fatalf("a locked profile can write: exit %d", blocked.code)
	}
	mustRun(t, "profile", "remove", "fresh")
	if listed := mustRun(t, "profile", "list"); strings.Contains(listed, "fresh") {
		t.Fatalf("the removed profile is still listed:\n%s", listed)
	}

	t.Setenv("TEST_ADMIN_EMAIL", adminEmail)
	t.Setenv("TEST_ADMIN_PASSWORD", testPassword)
	mustRun(t, "profile", "add", "from-env", "--url", pocketBase.url, "--identity-env", "TEST_ADMIN_EMAIL", "--password-env", "TEST_ADMIN_PASSWORD", "--header-env", "X-Proxy-Token=TEST_ADMIN_PASSWORD")
	if listed := mustRun(t, "profile", "list"); !strings.Contains(listed, "(identity from $TEST_ADMIN_EMAIL)") {
		t.Fatalf("unexpected profile list:\n%s", listed)
	}
	if status := mustRun(t, "-p", "from-env", "status"); !strings.Contains(status, adminEmail) || !strings.Contains(status, "mode: read-only") {
		t.Fatalf("the profile with its identity in the environment did not log in read-only:\n%s", status)
	}
	for _, refused := range [][]string{
		{"--header-env", "Authorization=TEST_ADMIN_PASSWORD"},
		{"--header-env", "X-Pbctl-As=TEST_ADMIN_PASSWORD"},
		{"--header-env", "X-Proxy-Token"},
		{"--identity-env", "TEST_ADMIN_EMAIL"},
	} {
		if outcome := run(t, "", append([]string{"profile", "add", "refused", "--url", pocketBase.url}, refused...)...); outcome.code != 2 {
			t.Errorf("profile add %s exited %d, want 2", strings.Join(refused, " "), outcome.code)
		}
	}

	t.Setenv("PBCTL_URL", pocketBase.url)
	t.Setenv("PBCTL_IDENTITY", adminEmail)
	t.Setenv("PBCTL_PASSWORD", testPassword)
	t.Setenv("PBCTL_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	if status := mustRun(t, "status"); !strings.Contains(status, adminEmail) {
		t.Fatalf("the environment profile did not log in:\n%s", status)
	}
}

func TestTheGatewayCommandServesAReadOnlyViewOfAnUpstream(t *testing.T) {
	pocketBase := startPocketBase(t, guardedByHook)
	recordID := seedPosts(t)
	listen := freeAddress(t)
	t.Setenv("PBCTL_GATEWAY_PASSWORD", testPassword)

	ctx, stop := context.WithCancel(context.Background())
	gatewayLog := &bytes.Buffer{}
	stopped := make(chan int, 1)
	go func() {
		stopped <- cli.Run(ctx, "test", []string{"gateway", "--listen", listen, "--upstream", pocketBase.url, "--identity", adminEmail}, strings.NewReader(""), io.Discard, gatewayLog)
	}()
	t.Cleanup(func() {
		stop()
		<-stopped
		if t.Failed() {
			t.Logf("gateway log:\n%s", gatewayLog.String())
		}
	})
	(&instance{t: t, url: "http://" + listen}).waitUntilHealthy()
	pocketBase.addProfile("viaGateway", map[string]any{"url": "http://" + listen, "read_only": false})

	if listed := mustRun(t, "-p", "viaGateway", "records", "list", "posts", "--fields", "id,title"); !strings.Contains(listed, recordID) {
		t.Fatalf("no rows through the gateway command:\n%s", listed)
	}
	if refused := run(t, "", "-p", "viaGateway", "records", "delete", "posts", recordID, "--yes"); refused.code != exitBlocked {
		t.Fatalf("a delete through the gateway command exited %d: %s", refused.code, refused.stderr)
	}
	stop()
	if code := <-stopped; code != 0 {
		t.Fatalf("the gateway exited %d on shutdown:\n%s", code, gatewayLog.String())
	}
	stopped <- 0
	if wrong := run(t, "", "gateway", "--listen", freeAddress(t), "--upstream", pocketBase.url, "--identity", adminEmail+"x"); wrong.code != exitAuth {
		t.Fatalf("a gateway with a wrong login exited %d, want %d: %s", wrong.code, exitAuth, wrong.stderr)
	}
}

func buildBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "pbctl")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/pbctl").CombinedOutput(); err != nil {
		t.Fatalf("build pbctl: %v\n%s", err, out)
	}
	return binary
}

func connectToBinary(t *testing.T, binary, profile string) *mcp.ClientSession {
	t.Helper()
	command := exec.Command(binary, "mcp", "-p", profile)
	command.Env = os.Environ()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "test"}, nil).Connect(t.Context(), &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func call(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) (string, bool) {
	t.Helper()
	outcome, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	text := &strings.Builder{}
	for _, content := range outcome.Content {
		if part, isText := content.(*mcp.TextContent); isText {
			text.WriteString(part.Text)
		}
	}
	return text.String(), outcome.IsError
}

func mustCall(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any, expected string) string {
	t.Helper()
	text, failed := call(t, session, name, arguments)
	if failed || !strings.Contains(text, expected) {
		t.Fatalf("tool %s (%v) failed=%v, want %q in:\n%s", name, arguments, failed, expected, text)
	}
	return text
}

func offeredTools(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func TestEveryMCPToolWorksThroughTheBuiltBinaryAgainstARealServer(t *testing.T) {
	startPocketBase(t, guardedByHook)
	firstID := seedPosts(t)
	attachment := filepath.Join(t.TempDir(), "plan.txt")
	os.WriteFile(attachment, []byte("plan"), 0o600)
	mustRun(t, "records", "update", "posts", firstID, "--file", "doc="+attachment)
	binary := buildBinary(t)
	session := connectToBinary(t, binary, "admin")

	wantTools := []string{
		"api_get", "api_write", "collections_list", "collections_show", "files_list", "guide", "logs_get", "logs_list",
		"records_count", "records_create", "records_delete", "records_get", "records_list", "records_update", "settings_get", "status",
	}
	if offered := offeredTools(t, session); !slices.Equal(offered, wantTools) {
		t.Fatalf("offered tools = %v\nwant            %v", offered, wantTools)
	}

	mustCall(t, session, "status", nil, "mode: writable")
	mustCall(t, session, "guide", nil, "Filter syntax")
	mustCall(t, session, "collections_list", nil, "posts\tbase\t")
	mustCall(t, session, "collections_list", map[string]any{"system": true}, "_superusers")
	mustCall(t, session, "collections_show", map[string]any{"collection": "posts"}, "title text required")
	mustCall(t, session, "records_list", map[string]any{"collection": "posts", "filter": "views > 5", "fields": "id,title", "sort": "-views", "perPage": 1, "page": 1}, firstID+"\tO'Brien's plan")
	mustCall(t, session, "records_get", map[string]any{"collection": "posts", "id": firstID, "fields": "title"}, "title: O'Brien's plan")
	mustCall(t, session, "records_count", map[string]any{"collection": "posts", "filter": "views >= 1"}, "2")
	mustCall(t, session, "files_list", map[string]any{"collection": "posts", "id": firstID}, "doc\tplan_")
	mustCall(t, session, "settings_get", map[string]any{"section": "batch"}, "batch.enabled: true")
	mustCall(t, session, "api_get", map[string]any{"path": "/api/collections/posts/records", "query": map[string]any{"perPage": "1", "fields": "title"}}, `"totalItems":2`)

	created := mustCall(t, session, "records_create", map[string]any{"collection": "posts", "data": map[string]any{"title": "made over MCP", "views": 3}}, "title: made over MCP")
	createdID := regexp.MustCompile(`(?m)^id: (\S+)$`).FindStringSubmatch(created)[1]
	mustCall(t, session, "records_update", map[string]any{"collection": "posts", "id": createdID, "data": map[string]any{"views+": 4}}, "views: 7")
	if text, failed := call(t, session, "records_delete", map[string]any{"collection": "posts", "ids": []string{createdID}, "confirm": false}); !failed || !strings.Contains(text, "confirm: true") {
		t.Fatalf("a delete without confirm went through: %s", text)
	}
	mustCall(t, session, "records_delete", map[string]any{"collection": "posts", "ids": []string{createdID}, "confirm": true}, "deleted posts/"+createdID)
	mustCall(t, session, "api_write", map[string]any{"method": "post", "path": "/api/collections/posts/records", "body": map[string]any{"title": "made over api_write"}}, "made over api_write")
	if text, failed := call(t, session, "api_write", map[string]any{"method": "DELETE", "path": "/api/collections/posts/records/" + firstID}); !failed || !strings.Contains(text, "confirm: true") {
		t.Fatalf("an unconfirmed DELETE over api_write went through: %s", text)
	}
	mustCall(t, session, "records_count", map[string]any{"collection": "posts"}, "3")

	logs := ""
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline) && !strings.Contains(logs, "\tINFO\t"); time.Sleep(250 * time.Millisecond) {
		logs, _ = call(t, session, "logs_list", map[string]any{"level": "info", "since": "1h", "status": "2xx", "perPage": 1})
	}
	lines := strings.Split(logs, "\n")
	if len(lines) < 2 || !strings.Contains(lines[1], "\tINFO\t") {
		t.Fatalf("logs_list returned:\n%s", logs)
	}
	mustCall(t, session, "logs_get", map[string]any{"id": strings.Fields(lines[1])[0]}, "level: 0")

	if text, failed := call(t, session, "records_list", map[string]any{"collection": "missing"}); !failed || !strings.Contains(text, "404") {
		t.Fatalf("a missing collection did not come back as an error: %s", text)
	}

	readOnly := connectToBinary(t, binary, "locked")
	offered := offeredTools(t, readOnly)
	for _, writing := range []string{"records_create", "records_update", "records_delete", "api_write"} {
		if slices.Contains(offered, writing) {
			t.Errorf("a read-only server offers %s", writing)
		}
	}
	if len(offered) != len(wantTools)-4 {
		t.Fatalf("a read-only server offers %d tools: %v", len(offered), offered)
	}
	mustCall(t, readOnly, "status", nil, "mode: read-only")
	mustCall(t, readOnly, "records_count", map[string]any{"collection": "posts"}, strconv.Itoa(3))
}
