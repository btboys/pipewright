package build

// ssh_exec.go 把画布「SSH 执行」节点(ssh_exec)真实化:在节点选定的目标服务器上,以该服务器登记的
// 登录用户 + 凭据经 SSH 登录,执行节点配置的多行命令。
//
// 与 jobConfigSchema 的 ssh_exec 字段一一对应:
//   - serverId       (必填) 目标服务器 id(target 域已登记的服务器);
//   - user           (可选) 执行用户:非空且与服务器登录用户不同 → `sudo -n -u <user> -H -- sh -c <script>`;
//     留空、或恰好等于登录用户 → 直接以登录用户身份执行(不套 sudo);
//   - commands       (必填) 多行命令 → `set -e` 单脚本(与 script 节点同款:cd/变量可跨行,任一行失败即失败);
//   - timeoutSeconds (可选) >0 时套 context.WithTimeout,超时即该节点失败。
//
// 安全边界:
//   - 命令经 target.Exec 以 array 传入,平台侧绝不拼 host shell(AC-SEC-02);用户命令只在远端 `sh -c`
//     内解释(与 script 节点「容器内 sh 解释」同一既定模式);
//   - 「执行用户」走白名单校验,非法值直接失败,绝不喂给 sudo(防 `-i`/`--`/`root:x` 之类怪值);
//   - 命令正文会回显进 run 日志(可观测性取舍),故前端 hint 明确警告「切勿写明文密钥」;
//   - 每次执行写一条 append-only 审计(Action=ssh_exec / TargetType=server),Detail 只存摘要;
//   - 配置缺失、服务未注入、服务器不存在、保险库未配置、认证失败、不可达 → **一律诚实失败**
//     (命令节点的「假成功」比假失败危险:用户会以为命令跑了)。
//
// 多台批量/流式日志/重试均为后续增量;本版单台、缓冲式输出。

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/btboys/pipewright/internal/audit"
	"github.com/btboys/pipewright/internal/dagrun"
	"github.com/btboys/pipewright/internal/pipeline"
	"github.com/btboys/pipewright/internal/run"
	"github.com/btboys/pipewright/internal/target"
)

// sshExecer 抽象「SSH 执行」节点所需的最小 target 能力(target.Service 即满足;便于 fake 单测):
// Get 取服务器视图(登录用户),Exec 以 array 命令执行。刻意不引入完整 target.Service,保持节点侧窄依赖。
type sshExecer interface {
	Get(ctx context.Context, id string) (*target.Server, error)
	Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error)
}

const (
	// sshExecUserPattern 是「执行用户」白名单:POSIX 用户名惯例(字母/下划线开头;后续字母数字与 _ . -),
	// 总长 ≤32。非法值一律失败 —— 虽然命令是 array 传参(不拼 shell),也不把怪值送进 sudo 的选项位。
	sshExecUserPattern = `^[A-Za-z_][A-Za-z0-9_.-]{0,31}$`
	// sshExecLogChunkLines 是单条日志记录携带的最大行数:运行日志 UI 支持多行文本(与 deploy cmdlog 同款),
	// 分块只为压住「一行一次 DB 写」的写放大。
	sshExecLogChunkLines = 100
	// sshExecLogChunkBytes 是单条日志记录的目标字节上限(与行数上限取先到者)。
	sshExecLogChunkBytes = 16 << 10
	// sshExecLogMaxLines 是每路输出写进节点日志的最大行数,超出只保留前 N 行 + 一行省略提示。
	// 缓冲式输出可能有几十万行,全量落库既慢又无排查价值;内存侧由 target 的单流上限兜底。
	sshExecLogMaxLines = 500
	// sshExecLogMaxBytes 是每路输出写进节点日志的字节上限。**必须与行数上限并存**:远端完全可以
	// 吐一条没有换行的巨长行(单行 base64 / JSON),只数行的话一条日志记录就吃掉整个 8 MiB 单流上限,
	// 撑爆 run_logs 行、SSE 事件与前端渲染。
	sshExecLogMaxBytes = 512 << 10 // 512 KiB / 流
	// sshExecAuditHeadRunes 是审计 Detail 里命令摘要的截断长度(按 rune 计,不切坏多字节字符)。
	// 审计表不是日志:只留足以追溯「跑的是什么」的头部。
	sshExecAuditHeadRunes = 200
)

var sshExecUserRe = regexp.MustCompile(sshExecUserPattern)

// isSSHExecJob 判断是否「SSH 执行」节点。
func isSSHExecJob(jobType string) bool {
	return strings.TrimSpace(jobType) == "ssh_exec"
}

// sshExecArgv 组装远端 argv:多行命令合成 `set -e` 单脚本,再按执行用户决定是否切换身份。
//
// 三个分支:
//   - user 空        → 直接以登录用户执行;
//   - user == 登录用户 → 同上(跳过 sudo:登录即 root 的精简机器不依赖远端 sudo/NOPASSWD);
//   - 其余           → `sudo -n -u <user> -H -- sh -c <script>`:-n 让「需要密码」立即失败而不是挂在
//     密码提示上,-H 让 HOME 指向目标用户,-- 断选项。
func sshExecArgv(lines []string, user, loginUser string) []string {
	script := "set -e\n" + strings.Join(lines, "\n")
	if user == "" || user == strings.TrimSpace(loginUser) {
		return []string{"sh", "-c", script}
	}
	return []string{"sudo", "-n", "-u", user, "-H", "--", "sh", "-c", script}
}

// runSSHExecJob 执行一个 SSH 执行节点。任一环节不满足 → ErrBuildFailed(阻断下游);上层取消 →
// run.ErrCanceled(尊重取消语义,不误报为执行失败)。
func (b *Builder) runSSHExecJob(ctx context.Context, rep dagrun.StageReporter, jb pipeline.Job, r *run.Run) error {
	if canceled(ctx) {
		return run.ErrCanceled
	}
	if b.sshExec == nil {
		_ = rep.Log(ctx, streamStderr, fmt.Sprintf("SSH 执行「%s」:SSH 执行服务未注入,节点失败", jb.Name))
		return ErrBuildFailed
	}

	serverID := cfgString(jb.Config, "serverId")
	if serverID == "" {
		_ = rep.Log(ctx, streamStderr, fmt.Sprintf("SSH 执行「%s」未选目标服务器(serverId 空)", jb.Name))
		return ErrBuildFailed
	}
	// 「执行用户」取原值(不做 {{}} 渲染):一个需要模板的用户名本身就是配置错误,校验会据实报错。
	user := cfgString(jb.Config, "user")
	if user != "" && !sshExecUserRe.MatchString(user) {
		_ = rep.Log(ctx, streamStderr, fmt.Sprintf(
			"SSH 执行「%s」执行用户非法(%q):只允许字母/下划线开头,后续字母数字与 _ . -,长度 ≤32", jb.Name, user))
		return ErrBuildFailed
	}
	// 命令支持 {{key}} 模板(与 script 节点同一套渲染上下文:节点 config 字符串值 + 自由 params 表)。
	lines := splitCommands(renderTemplate(cfgString(jb.Config, "commands"), templateContext(jb.Config)))
	if len(lines) == 0 {
		_ = rep.Log(ctx, streamStderr, fmt.Sprintf("SSH 执行「%s」缺少执行命令(commands)", jb.Name))
		return ErrBuildFailed
	}

	srv, err := b.sshExec.Get(ctx, serverID)
	if err != nil {
		_ = rep.Log(ctx, streamStderr, "SSH 执行:目标服务器不可用:"+humanSSHExecErr(err))
		return ErrBuildFailed
	}
	loginUser := strings.TrimSpace(srv.User)
	argv := sshExecArgv(lines, user, loginUser)
	timeout := cfgNonNegInt(jb.Config, "timeoutSeconds")

	// 执行前回显:目标身份 + 命令正文。命令正文入 run 日志是可观测性的取舍(用户据此确认到底跑了什么),
	// 前端 hint 因此明确警告「切勿在此写明文密钥」。
	_ = rep.Log(ctx, streamStdout, fmt.Sprintf("→ SSH 执行:%s,以 %s 执行 %d 行命令",
		sshExecTargetLabel(srv), sshExecWhoLabel(user, loginUser), len(lines)))
	_ = rep.Log(ctx, streamStdout, "$ "+strings.Join(argv, " "))
	if timeout > 0 {
		_ = rep.Log(ctx, streamStdout, fmt.Sprintf("超时上限 %d 秒(超时即判本节点失败)", timeout))
	}

	// 审计在**发起前**写:记录「谁要在哪台机上以哪个用户跑什么」的意图。连接失败也不丢这条记录
	// (终端会话审计写在握手成功后;命令执行无法在失败后补记,故前移)。
	b.recordSSHExecAudit(ctx, r, jb, serverID, user, lines)

	execCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}
	res, execErr := b.sshExec.Exec(execCtx, serverID, argv)
	if execErr != nil {
		// 上层取消(用户取消/运行级超时)优先:不算本节点的执行失败。
		if ctx.Err() != nil {
			return run.ErrCanceled
		}
		if errors.Is(execErr, context.DeadlineExceeded) || errors.Is(execErr, context.Canceled) {
			_ = rep.Log(ctx, streamStderr, fmt.Sprintf("SSH 执行超时(超过 %d 秒):远端命令可能仍在运行", timeout))
			return ErrBuildFailed
		}
		_ = rep.Log(ctx, streamStderr, "SSH 执行失败:"+humanSSHExecErr(execErr))
		return ErrBuildFailed
	}
	if res == nil {
		_ = rep.Log(ctx, streamStderr, "SSH 执行失败:未拿到执行结果")
		return ErrBuildFailed
	}

	logSSHExecOutput(ctx, rep, streamStdout, res.Stdout)
	logSSHExecOutput(ctx, rep, streamStderr, res.Stderr)
	if res.Truncated {
		_ = rep.Log(ctx, streamStderr, "警告:远端输出超过平台单路上限,回传内容已截断(远端命令本身未受影响)")
	}
	if res.ExitCode != 0 {
		_ = rep.Log(ctx, streamStderr, fmt.Sprintf("✗ 远端命令退出码 %d", res.ExitCode))
		if hint := sshExecSudoHint(user, res.Stderr); hint != "" {
			_ = rep.Log(ctx, streamStderr, hint)
		}
		return ErrBuildFailed
	}
	_ = rep.Log(ctx, streamStdout, "✓ SSH 执行完成")
	return nil
}

// recordSSHExecAudit 写一条 ssh_exec 审计(高危:任意命令在目标机落地)。auditor 未注入时静默跳过
// (审计是附加能力,不阻断执行)。Detail 只存摘要,绝不含完整输出;命令头部过 Masker 后才入库。
func (b *Builder) recordSSHExecAudit(ctx context.Context, r *run.Run, jb pipeline.Job, serverID, user string, lines []string) {
	if b.auditor == nil {
		return
	}
	var actor, runID string
	if r != nil {
		actor, runID = r.Trigger.Actor, r.ID
	}
	_ = b.auditor.Record(ctx, audit.Entry{
		Actor:      actor,
		Action:     audit.ActionSSHExec,
		TargetType: audit.TargetServer,
		TargetID:   serverID,
		Detail: map[string]any{
			"runId":       runID,
			"jobId":       jb.ID,
			"jobName":     jb.Name,
			"user":        user, // 空 = 以服务器登记的登录用户直接执行
			"lines":       len(lines),
			"commandHead": truncateRunes(strings.Join(lines, "\n"), sshExecAuditHeadRunes),
		},
	})
}

// logSSHExecOutput 把远端某路输出写进节点日志:先按「行数 + 字节数」双重上限截出前缀,
// 再按 sshExecLogChunkLines / sshExecLogChunkBytes 合批写(压住 DB 写放大),被省略时补一行如实提示。
func logSSHExecOutput(ctx context.Context, rep dagrun.StageReporter, stream, text string) {
	trimmed := strings.TrimRight(text, "\r\n")
	if strings.TrimSpace(trimmed) == "" {
		return
	}
	prefix, elided := sshExecLogPrefix(trimmed)
	if prefix != "" {
		for _, chunk := range chunkSSHExecLog(prefix) {
			_ = rep.Log(ctx, stream, chunk)
		}
	}
	if elided {
		_ = rep.Log(ctx, stream, fmt.Sprintf("…(输出过长:节点日志仅保留前 %d 行 / %d KiB)",
			sshExecLogMaxLines, sshExecLogMaxBytes>>10))
	}
}

// sshExecLogPrefix 按「行数 + 字节数」双重上限截出要落日志的前缀,返回前缀文本与是否发生省略。
// 边界的两种形态都要处理:行很多(截到行数上限)、单行极长(截到字节上限,保留该行的前缀而不是整体丢弃)。
func sshExecLogPrefix(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	var b strings.Builder
	kept := 0
	for _, line := range lines {
		if kept >= sshExecLogMaxLines || (b.Len() > 0 && b.Len()+1+len(line) > sshExecLogMaxBytes) {
			return b.String(), true
		}
		if len(line) > sshExecLogMaxBytes {
			// 单行本身就超过整路上限(没有换行的巨长输出):保留其前缀,不让这一行撑爆日志。
			if kept > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(truncateBytes(line, sshExecLogMaxBytes) + "…")
			return b.String(), true
		}
		if kept > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		kept++
	}
	return b.String(), false
}

// chunkSSHExecLog 把前缀文本切成若干条日志记录(每块 ≤ sshExecLogChunkLines 行且 ≤ sshExecLogChunkBytes 字节)。
func chunkSSHExecLog(prefix string) []string {
	lines := strings.Split(prefix, "\n")
	chunks := make([]string, 0, len(lines)/sshExecLogChunkLines+1)
	var cur []string
	curBytes := 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		chunks = append(chunks, strings.Join(cur, "\n"))
		cur = nil
		curBytes = 0
	}
	for _, line := range lines {
		cur = append(cur, line)
		curBytes += len(line) + 1
		if len(cur) >= sshExecLogChunkLines || curBytes >= sshExecLogChunkBytes {
			flush()
		}
	}
	flush()
	return chunks
}

// truncateBytes 按字节上限截断(不切坏多字节字符)。
func truncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := s[:maxBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// sshExecTargetLabel 拼人读目标标签(名称 + 登录身份 + 地址);绝不含凭据。
func sshExecTargetLabel(srv *target.Server) string {
	if srv == nil {
		return "(未知服务器)"
	}
	return fmt.Sprintf("%s(%s@%s:%d)", srv.Name, srv.User, srv.Host, srv.Port)
}

// sshExecWhoLabel 拼人读执行身份:与登录用户相同(或留空)时说明「未套 sudo」,否则显示 sudo 形态。
func sshExecWhoLabel(user, loginUser string) string {
	if user == "" || user == strings.TrimSpace(loginUser) {
		if loginUser == "" {
			return "登录用户(未套 sudo)"
		}
		return loginUser + "(登录用户,未套 sudo)"
	}
	return "sudo -u " + user
}

// sshExecSudoHint 在远端 sudo 因缺免密/无授权而失败时补一行可操作提示(sudo -n 的原始报错对用户不直观)。
// 只在退出码非零且有 sudo 特征时给,避免误导。
func sshExecSudoHint(user, stderr string) string {
	if user == "" {
		return ""
	}
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "a password is required"),
		strings.Contains(s, "a terminal is required"),
		strings.Contains(s, "no tty present"),
		strings.Contains(s, "askpass"):
		return fmt.Sprintf("提示:远端 sudo 需要密码/终端,请为 %s 配置免密(visudo 的 NOPASSWD),或把「执行用户」留空以登录用户执行", user)
	case strings.Contains(s, "is not in the sudoers"), strings.Contains(s, "not allowed to execute"):
		return fmt.Sprintf("提示:%s 不在远端 sudoers 授权中,请为其授权或改用有权限的执行用户", user)
	}
	return ""
}

// humanSSHExecErr 把 target 领域错误映射成人读中文(领域错误本身不含凭据明文,绝无泄漏面)。
func humanSSHExecErr(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, target.ErrAuth):
		return "SSH 认证失败(密钥/口令无效,或该用户名下未授权此凭据)"
	case errors.Is(err, target.ErrVaultUnconfigured):
		return "保险库未配置 master key,无法取出该服务器的 SSH 凭据"
	case errors.Is(err, target.ErrCredentialNotFound):
		return "该服务器引用的 SSH 凭据不存在(可能已被删除)"
	case errors.Is(err, target.ErrInvalidCredential):
		return "该服务器绑定的凭据不是可用的 SSH 私钥/口令"
	case errors.Is(err, target.ErrNotFound):
		return "目标服务器不存在(可能已被删除)"
	case errors.Is(err, target.ErrUnreachable):
		return "无法连接目标服务器(端口关闭/主机不可达/超时)"
	default:
		return err.Error()
	}
}

// truncateRunes 按 rune 截断(不切坏多字节字符),超长时补省略号。
func truncateRunes(s string, maxRunes int) string {
	rs := []rune(s)
	if len(rs) <= maxRunes {
		return s
	}
	return string(rs[:maxRunes]) + "…"
}
