package build

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/btboys/pipewright/internal/audit"
	"github.com/btboys/pipewright/internal/dagrun"
	"github.com/btboys/pipewright/internal/pipeline"
	"github.com/btboys/pipewright/internal/run"
	"github.com/btboys/pipewright/internal/target"
)

// ─── 测试替身 ──────────────────────────────────────────────────────────────────

// stubSSHExec 记录 Get/Exec 调用并按配置返回结果/错误(不触网)。
type stubSSHExec struct {
	srv    *target.Server
	getErr error

	getCalls    int
	execCalls   int
	gotServerID string
	gotCmd      []string
	gotDeadline bool

	res *target.ExecResult
	err error
	// blockUntilCtxDone 模拟「远端命令挂住」:等 ctx 结束后返回 ctx.Err()(验证超时/取消语义)。
	blockUntilCtxDone bool
	// onExec 可选回调:记录实际下发的命令(断言 DAG 串行顺序)。
	onExec func(cmd []string)
}

func (s *stubSSHExec) Get(_ context.Context, _ string) (*target.Server, error) {
	s.getCalls++
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.srv, nil
}

func (s *stubSSHExec) Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	s.execCalls++
	s.gotServerID = serverID
	s.gotCmd = append([]string(nil), cmd...)
	_, s.gotDeadline = ctx.Deadline()
	if s.onExec != nil {
		s.onExec(s.gotCmd)
	}
	if s.blockUntilCtxDone {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.res != nil {
		return s.res, nil
	}
	return &target.ExecResult{}, nil
}

// stubAuditor 记录审计写入(audit.Recorder 的只读半边不参与本测试)。
type stubAuditor struct{ entries []audit.Entry }

func (a *stubAuditor) Record(_ context.Context, e audit.Entry) error {
	a.entries = append(a.entries, e)
	return nil
}

func (a *stubAuditor) List(context.Context, audit.ListFilter) (*audit.ListResult, error) {
	return &audit.ListResult{}, nil
}

// sshExecReporter 记录日志与节点终态(带 stream,便于断言 stdout/stderr 分流)。
type sshExecReporter struct {
	lines   []string
	streams []string
	done    []string
}

func (r *sshExecReporter) Log(_ context.Context, stream, line string) error {
	r.lines = append(r.lines, line)
	r.streams = append(r.streams, stream)
	return nil
}
func (r *sshExecReporter) EmitArtifact(context.Context, run.Artifact) error { return nil }
func (r *sshExecReporter) JobRunning(context.Context, string) error         { return nil }
func (r *sshExecReporter) JobDone(_ context.Context, jobID, status string) error {
	r.done = append(r.done, jobID+"="+status)
	return nil
}
func (r *sshExecReporter) JobReporter(string) dagrun.StageReporter { return r }

func (r *sshExecReporter) joined() string { return strings.Join(r.lines, "\n") }

func (r *sshExecReporter) logContains(sub string) bool { return strings.Contains(r.joined(), sub) }

// ─── sshExecArgv(纯函数)───────────────────────────────────────────────────────

func TestSSHExecArgv(t *testing.T) {
	lines := []string{"id", "echo hi"}
	wantScript := "set -e\nid\necho hi"

	cases := []struct {
		name      string
		user      string
		loginUser string
		want      []string
	}{
		{
			name: "执行用户留空:直接以登录用户执行(不套 sudo)",
			user: "", loginUser: "deploy",
			want: []string{"sh", "-c", wantScript},
		},
		{
			name: "执行用户等于登录用户:跳过 sudo(登录即 root 的精简机器不依赖 NOPASSWD)",
			user: "deploy", loginUser: "deploy",
			want: []string{"sh", "-c", wantScript},
		},
		{
			name: "执行用户不同:sudo -n -u <user> -H -- sh -c",
			user: "app", loginUser: "deploy",
			want: []string{"sudo", "-n", "-u", "app", "-H", "--", "sh", "-c", wantScript},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sshExecArgv(lines, c.user, c.loginUser); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("argv 不符\n got=%q\nwant=%q", got, c.want)
			}
		})
	}
}

// ─── runSSHExecJob(节点主流程)──────────────────────────────────────────────────

func TestRunSSHExecJob(t *testing.T) {
	srv := &target.Server{ID: "srv-1", Name: "web-01", Host: "10.0.0.1", Port: 22, User: "deploy"}
	okRes := func(stdout, stderr string, code int) *target.ExecResult {
		return &target.ExecResult{Stdout: stdout, Stderr: stderr, ExitCode: code}
	}

	cases := []struct {
		name string

		config      map[string]any
		srv         *target.Server
		getErr      error
		res         *target.ExecResult
		execErr     error
		noService   bool
		noAuditor   bool
		cancelFirst bool
		block       bool

		wantErr         error
		wantExecCalls   int
		wantServerID    string
		wantArgv        []string
		wantDeadline    bool
		wantAudit       int
		wantAuditUser   string
		wantLogContains []string
		wantLogMissing  []string
	}{
		{
			name:          "留空执行用户:成功执行并回写 stdout",
			config:        map[string]any{"serverId": "srv-1", "commands": "id\nwhoami"},
			srv:           srv,
			res:           okRes("uid=0(root)\nroot\n", "", 0),
			wantExecCalls: 1, wantServerID: "srv-1",
			wantArgv:  []string{"sh", "-c", "set -e\nid\nwhoami"},
			wantAudit: 1, wantAuditUser: "",
			wantLogContains: []string{
				"web-01(deploy@10.0.0.1:22)", "deploy(登录用户,未套 sudo)", "执行 2 行命令",
				"$ sh -c set -e", "uid=0(root)", "✓ SSH 执行完成",
			},
		},
		{
			name:          "指定执行用户:argv 走 sudo -n 并回显 sudo 形态",
			config:        map[string]any{"serverId": "srv-1", "user": "app", "commands": "whoami"},
			srv:           srv,
			res:           okRes("app\n", "", 0),
			wantExecCalls: 1,
			wantArgv:      []string{"sudo", "-n", "-u", "app", "-H", "--", "sh", "-c", "set -e\nwhoami"},
			wantAudit:     1, wantAuditUser: "app",
			wantLogContains: []string{"sudo -u app", "app\n"},
		},
		{
			name:            "执行用户等于登录用户:跳过 sudo 而不是 sudo -u root",
			config:          map[string]any{"serverId": "srv-1", "user": "deploy", "commands": "whoami"},
			srv:             srv,
			res:             okRes("deploy\n", "", 0),
			wantExecCalls:   1,
			wantArgv:        []string{"sh", "-c", "set -e\nwhoami"},
			wantAudit:       1,
			wantAuditUser:   "deploy",
			wantLogContains: []string{"deploy(登录用户,未套 sudo)"},
		},
		{
			name:          "命令里的 {{param}} 按自由参数渲染",
			config:        map[string]any{"serverId": "srv-1", "commands": "chown {{owner}} /opt/app", "params": "owner=app"},
			srv:           srv,
			res:           okRes("", "", 0),
			wantExecCalls: 1,
			wantArgv:      []string{"sh", "-c", "set -e\nchown app /opt/app"},
			wantAudit:     1,
		},
		{
			name:            "未选目标服务器:诚实失败,不触网",
			config:          map[string]any{"commands": "id"},
			srv:             srv,
			wantErr:         ErrBuildFailed,
			wantAudit:       0,
			wantLogContains: []string{"未选目标服务器"},
		},
		{
			name:            "命令为空:诚实失败,不触网",
			config:          map[string]any{"serverId": "srv-1", "commands": "   \n\n"},
			srv:             srv,
			wantErr:         ErrBuildFailed,
			wantLogContains: []string{"缺少执行命令"},
		},
		{
			name:          "执行用户非法:失败且绝不喂给 sudo",
			config:        map[string]any{"serverId": "srv-1", "user": "-i root", "commands": "id"},
			srv:           srv,
			wantErr:       ErrBuildFailed,
			wantExecCalls: 0, wantAudit: 0,
			wantLogContains: []string{"执行用户非法"},
		},
		{
			name:            "服务器不存在:人读失败",
			config:          map[string]any{"serverId": "gone", "commands": "id"},
			getErr:          target.ErrNotFound,
			wantErr:         ErrBuildFailed,
			wantAudit:       0,
			wantLogContains: []string{"目标服务器不存在"},
		},
		{
			name:            "服务未注入:诚实失败(不冒充执行成功)",
			config:          map[string]any{"serverId": "srv-1", "commands": "id"},
			noService:       true,
			wantErr:         ErrBuildFailed,
			wantLogContains: []string{"SSH 执行服务未注入"},
		},
		{
			name:          "远端非零退出:节点失败并回写 stderr/退出码",
			config:        map[string]any{"serverId": "srv-1", "commands": "false"},
			srv:           srv,
			res:           okRes("", "boom\n", 3),
			wantErr:       ErrBuildFailed,
			wantExecCalls: 1, wantAudit: 1,
			wantLogContains: []string{"boom", "✗ 远端命令退出码 3"},
		},
		{
			name:            "sudo 缺免密:补一行可操作提示",
			config:          map[string]any{"serverId": "srv-1", "user": "app", "commands": "id"},
			srv:             srv,
			res:             okRes("", "sudo: a password is required\n", 1),
			wantErr:         ErrBuildFailed,
			wantExecCalls:   1,
			wantAudit:       1,
			wantAuditUser:   "app",
			wantLogContains: []string{"NOPASSWD"},
		},
		{
			name:          "认证失败:映射为人读原因(不含凭据)",
			config:        map[string]any{"serverId": "srv-1", "commands": "id"},
			srv:           srv,
			execErr:       target.ErrAuth,
			wantErr:       ErrBuildFailed,
			wantExecCalls: 1, wantAudit: 1,
			wantLogContains: []string{"SSH 认证失败"},
		},
		{
			name:            "输出被截断:如实提示",
			config:          map[string]any{"serverId": "srv-1", "commands": "cat huge"},
			srv:             srv,
			res:             &target.ExecResult{Stdout: "head...", ExitCode: 0, Truncated: true},
			wantExecCalls:   1,
			wantAudit:       1,
			wantLogContains: []string{"已截断"},
		},
		{
			name:            "超时:套上 deadline 并按超时失败",
			config:          map[string]any{"serverId": "srv-1", "commands": "sleep 999", "timeoutSeconds": "1"},
			srv:             srv,
			block:           true,
			wantErr:         ErrBuildFailed,
			wantExecCalls:   1,
			wantAudit:       1,
			wantDeadline:    true,
			wantLogContains: []string{"超时上限 1 秒", "SSH 执行超时"},
		},
		{
			name:          "上层取消:归一为取消而非执行失败",
			config:        map[string]any{"serverId": "srv-1", "commands": "sleep 999"},
			srv:           srv,
			cancelFirst:   true,
			wantErr:       run.ErrCanceled,
			wantExecCalls: 0,
		},
		{
			name:            "审计未注入:执行照常(审计不阻断)",
			config:          map[string]any{"serverId": "srv-1", "commands": "id"},
			srv:             srv,
			res:             okRes("ok\n", "", 0),
			noAuditor:       true,
			wantExecCalls:   1,
			wantLogContains: []string{"✓ SSH 执行完成"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := &stubSSHExec{srv: c.srv, getErr: c.getErr, res: c.res, err: c.execErr, blockUntilCtxDone: c.block}
			aud := &stubAuditor{}
			b := &Builder{sshExec: stub, auditor: aud}
			if c.noService {
				b.sshExec = nil
			}
			if c.noAuditor {
				b.auditor = nil
			}
			rep := &sshExecReporter{}
			jb := pipeline.Job{ID: "j1", Name: "运维命令", Type: "ssh_exec", Config: c.config}
			r := &run.Run{ID: "run-1", Trigger: run.Trigger{Actor: "alice"}}

			ctx := context.Background()
			if c.cancelFirst {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			err := b.runSSHExecJob(ctx, rep, jb, r)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v,want %v;logs=\n%s", err, c.wantErr, rep.joined())
			}
			if stub.execCalls != c.wantExecCalls {
				t.Fatalf("exec 调用 %d 次,want %d", stub.execCalls, c.wantExecCalls)
			}
			if c.wantServerID != "" && stub.gotServerID != c.wantServerID {
				t.Fatalf("serverID = %q,want %q", stub.gotServerID, c.wantServerID)
			}
			if c.wantArgv != nil && !reflect.DeepEqual(stub.gotCmd, c.wantArgv) {
				t.Fatalf("argv 不符\n got=%q\nwant=%q", stub.gotCmd, c.wantArgv)
			}
			if c.wantDeadline && !stub.gotDeadline {
				t.Fatal("应带上 deadline(超时字段未生效)")
			}
			if c.wantDeadline == false && stub.gotDeadline {
				t.Fatal("未配超时时不应带 deadline")
			}
			if len(aud.entries) != c.wantAudit {
				t.Fatalf("审计写入 %d 条,want %d", len(aud.entries), c.wantAudit)
			}
			if c.wantAudit > 0 {
				e := aud.entries[0]
				if e.Action != audit.ActionSSHExec || e.TargetType != audit.TargetServer || e.TargetID != "srv-1" {
					t.Fatalf("审计条目不符:%+v", e)
				}
				if e.Actor != "alice" {
					t.Fatalf("审计 actor = %q,want alice", e.Actor)
				}
				if got, _ := e.Detail["user"].(string); got != c.wantAuditUser {
					t.Fatalf("审计 user = %q,want %q", got, c.wantAuditUser)
				}
				if got, _ := e.Detail["runId"].(string); got != "run-1" {
					t.Fatalf("审计 runId = %q,want run-1", got)
				}
				if head, _ := e.Detail["commandHead"].(string); head == "" {
					t.Fatal("审计应带命令摘要(非空)")
				}
			}
			for _, want := range c.wantLogContains {
				if !rep.logContains(want) {
					t.Fatalf("日志缺少 %q;logs=\n%s", want, rep.joined())
				}
			}
		})
	}
}

// TestRunSSHExecJobLogBounds 断言缓冲式输出的日志边界:合批(不逐行写)、每路截到上限、
// 超出部分如实提示,stdout/stderr 分流。
func TestRunSSHExecJobLogBounds(t *testing.T) {
	const total = sshExecLogMaxLines + 40
	var sb strings.Builder
	for i := 0; i < total; i++ {
		fmt.Fprintf(&sb, "line-%d\n", i)
	}
	stub := &stubSSHExec{
		srv: &target.Server{ID: "srv-1", Name: "web-01", Host: "10.0.0.1", Port: 22, User: "deploy"},
		res: &target.ExecResult{Stdout: sb.String(), Stderr: "oops\n", ExitCode: 0},
	}
	b := &Builder{sshExec: stub, auditor: &stubAuditor{}}
	rep := &sshExecReporter{}
	jb := pipeline.Job{ID: "j1", Name: "看日志", Type: "ssh_exec",
		Config: map[string]any{"serverId": "srv-1", "commands": "cat app.log"}}

	if err := b.runSSHExecJob(context.Background(), rep, jb, &run.Run{ID: "run-1"}); err != nil {
		t.Fatalf("run: %v", err)
	}

	// 合批:输出行不该一条一条写日志(合批上限 100 行/条)。
	outRecords := 0
	for i, line := range rep.lines {
		if rep.streams[i] != streamStdout {
			continue
		}
		n := strings.Count(line, "\n") + 1
		if strings.HasPrefix(line, "line-") {
			outRecords++
			if n > sshExecLogChunkLines {
				t.Fatalf("单条日志超过合批上限:%d 行", n)
			}
		}
	}
	if want := sshExecLogMaxLines / sshExecLogChunkLines; outRecords != want {
		t.Fatalf("stdout 合批记录数 = %d,want %d", outRecords, want)
	}
	if !rep.logContains("输出过长:节点日志仅保留前") || !rep.logContains(fmt.Sprintf("%d 行", sshExecLogMaxLines)) {
		t.Fatalf("缺少输出省略提示;logs=\n%s", rep.joined())
	}
	// stderr 走 stderr 流(只有 "oops" 一行 + 完成行在 stdout)。
	stderrSeen := false
	for i, line := range rep.lines {
		if rep.streams[i] == streamStderr && strings.Contains(line, "oops") {
			stderrSeen = true
		}
	}
	if !stderrSeen {
		t.Fatalf("stderr 未走 stderr 流;streams=%v", rep.streams)
	}
}

// TestRunSSHExecJobLogBoundsSingleHugeLine 覆盖真机验证抓到的那类输出:**一条没有换行的巨长行**
// (远端 base64/JSON 一行吐满 8 MiB)。只数行的限流挡不住它 —— 一条日志记录就能吃掉整路输出,
// 撑爆 run_logs 行 / SSE 事件 / 前端渲染。断言:日志被字节上限截住、有如实提示、节点仍成功。
func TestRunSSHExecJobLogBoundsSingleHugeLine(t *testing.T) {
	huge := strings.Repeat("x", sshExecLogMaxBytes*3) // 远超日志字节上限的单行
	stub := &stubSSHExec{
		srv: &target.Server{ID: "srv-1", Name: "web-01", Host: "10.0.0.1", Port: 22, User: "deploy"},
		res: &target.ExecResult{Stdout: huge, ExitCode: 0, Truncated: true},
	}
	b := &Builder{sshExec: stub, auditor: &stubAuditor{}}
	rep := &sshExecReporter{}
	jb := pipeline.Job{ID: "j1", Name: "看巨长行", Type: "ssh_exec",
		Config: map[string]any{"serverId": "srv-1", "commands": "cat one-line.json"}}

	if err := b.runSSHExecJob(context.Background(), rep, jb, &run.Run{ID: "run-1"}); err != nil {
		t.Fatalf("run: %v", err)
	}

	total := 0
	for i, line := range rep.lines {
		if rep.streams[i] != streamStdout {
			continue
		}
		total += len(line)
		if strings.HasPrefix(line, "xxx") && len(line) > sshExecLogMaxBytes+8 {
			t.Fatalf("单条日志吃下 %d 字节,超过字节上限 %d(巨长行未被截住)", len(line), sshExecLogMaxBytes)
		}
	}
	if !rep.logContains("输出过长:节点日志仅保留前") {
		t.Fatalf("巨长行被截断时应有如实提示;logs 长度=%d", total)
	}
	if !rep.logContains("已截断") {
		t.Fatal("dialer 的 Truncated 标记应被提示")
	}
}

// ─── 派发接线:三条路径都要真跑(验收 #7 / #8)──────────────────────────────────

func sshExecTestJob(id, name string, needs ...string) pipeline.Job {
	return pipeline.Job{
		ID: id, Name: name, Type: "ssh_exec", Needs: needs,
		Config: map[string]any{"serverId": "srv-1", "commands": "echo " + name},
	}
}

// 类型分组串行路径(无 job 级 DAG):单 job 阶段应真实执行 ssh_exec。
func TestStageExecutorRunsSSHExecJob(t *testing.T) {
	stub := &stubSSHExec{srv: &target.Server{ID: "srv-1", User: "deploy"}, res: &target.ExecResult{ExitCode: 0}}
	b := &Builder{sshExec: stub}
	exec := NewStageExecutor(b, nil)

	rep := &sshExecReporter{}
	stage := pipeline.Stage{ID: "s1", Name: "运维", Kind: pipeline.KindDeploy, Jobs: []pipeline.Job{sshExecTestJob("j1", "重启服务")}}
	if err := exec(context.Background(), &run.Run{ID: "run-1"}, stage, rep); err != nil {
		t.Fatalf("stage exec: %v", err)
	}
	if stub.execCalls != 1 {
		t.Fatalf("ssh_exec 应真实执行一次,实际 %d", stub.execCalls)
	}
	if len(rep.done) != 1 || rep.done[0] != "j1=success" {
		t.Fatalf("节点终态不符:%v", rep.done)
	}
}

// job 级 DAG 路径:带 needs 的两个 ssh_exec 节点按依赖串行真跑。
func TestStageExecutorRunsSSHExecJobInJobDAG(t *testing.T) {
	var order []string
	stub := &stubSSHExec{srv: &target.Server{ID: "srv-1", User: "deploy"}, res: &target.ExecResult{ExitCode: 0}}
	stub.onExec = func(cmd []string) { order = append(order, cmd[len(cmd)-1]) }
	b := &Builder{sshExec: stub}
	exec := NewStageExecutor(b, nil)

	rep := &sshExecReporter{}
	stage := pipeline.Stage{ID: "s1", Name: "运维", Kind: pipeline.KindDeploy, Jobs: []pipeline.Job{
		sshExecTestJob("j1", "先重启"),
		sshExecTestJob("j2", "后校验", "j1"),
	}}
	if err := exec(context.Background(), &run.Run{ID: "run-1"}, stage, rep); err != nil {
		t.Fatalf("stage exec: %v", err)
	}
	if stub.execCalls != 2 {
		t.Fatalf("两个节点都应执行,实际 %d", stub.execCalls)
	}
	if len(order) != 2 || !strings.Contains(order[0], "先重启") || !strings.Contains(order[1], "后校验") {
		t.Fatalf("needs 串行顺序不符:%v", order)
	}
}

// 远程 runner 路径:ssh_exec 必须从控制机真跑(不能「本阶段放行」造成假成功)。
func TestStageRemoteRunsSSHExecJob(t *testing.T) {
	tgt := &fakeRemoteTarget{}
	stub := &stubSSHExec{srv: &target.Server{ID: "srv-1", User: "deploy"}, res: &target.ExecResult{ExitCode: 0}}
	b := &Builder{sshExec: stub}
	exec := NewStageExecutorWithRunner(b, nil, fakeRunnerLookup{serverID: "runner-1"}, tgt)

	rep := &sshExecReporter{}
	stage := pipeline.Stage{ID: "s1", Name: "运维", Kind: pipeline.KindDeploy, Jobs: []pipeline.Job{sshExecTestJob("j1", "清缓存")}}
	if err := exec(context.Background(), &run.Run{ID: "run-1"}, stage, rep); err != nil {
		t.Fatalf("remote stage exec: %v", err)
	}
	if stub.execCalls != 1 {
		t.Fatalf("ssh_exec 在远程 runner 模式也应从控制机执行一次,实际 %d", stub.execCalls)
	}
	if len(rep.done) != 1 || rep.done[0] != "j1=success" {
		t.Fatalf("节点终态不符:%v", rep.done)
	}
	if len(tgt.cmds) != 0 {
		t.Fatalf("纯 ssh_exec 阶段不该把命令下发到 runner,实际 %v", tgt.cmds)
	}
	if rep.logContains("本阶段放行") {
		t.Fatalf("不该放行;logs=\n%s", rep.joined())
	}
}
