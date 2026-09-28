package target

// exec_e2e_test.go 对真的 alpine+sshd 目标容器跑真 Exec(缓冲式执行),覆盖 fake 单验证不到的栈行为:
//   - stdout / stderr 分流与远端退出码如实回传;
//   - 多行脚本作为**单个 argv 参数**送达(平台侧 array,不拼 host shell);
//   - 元字符注入防护:参数里的 `; rm -rf` / `$(...)` 被当字面量,绝不被远端二次解释;
//   - **单流输出上限**:远端吐出超过上限的字节 → 内容截断 + Truncated 置位,且会话**不被误判为错误**。
//     最后一条是本文件的核心:x/crypto/ssh 内部走 io.Copy,若 limitedBuffer 对溢出字节返回短写,
//     io.Copy 会报 io.ErrShortWrite 并把「写满上限」当成会话失败(命令中断、退出码丢失)。
//
// 默认 SKIP:仅 PIPEWRIGHT_E2E_DEPLOY=1 且本机有 docker 时运行(CI 无 docker)。
// 跑法:PIPEWRIGHT_E2E_DEPLOY=1 go test ./internal/target/ -run E2EExecRealSSH -v

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestE2EExecRealSSH(t *testing.T) {
	requireE2EDeploy(t)
	c := startSSHContainer(t)
	addr := fmt.Sprintf("%s:%d", c.Host, c.Port)
	cfg := SSHConfig{User: "root", PrivateKey: c.PrivateKey}

	run := func(t *testing.T, script string) *ExecResult {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := sshDialer{}.Run(ctx, addr, cfg, []string{"sh", "-c", script})
		if err != nil {
			t.Fatalf("真 Exec 失败: %v", err)
		}
		if res == nil {
			t.Fatal("真 Exec 返回 nil 结果")
		}
		return res
	}

	t.Run("stdout/stderr 分流 + 退出码如实回传", func(t *testing.T) {
		res := run(t, "echo out-line; echo err-line >&2; exit 7")
		if res.ExitCode != 7 {
			t.Fatalf("退出码 = %d,want 7", res.ExitCode)
		}
		if !strings.Contains(res.Stdout, "out-line") {
			t.Fatalf("stdout 未捕获:%q", res.Stdout)
		}
		if !strings.Contains(res.Stderr, "err-line") {
			t.Fatalf("stderr 未捕获:%q", res.Stderr)
		}
		if strings.Contains(res.Stdout, "err-line") || strings.Contains(res.Stderr, "out-line") {
			t.Fatalf("stdout/stderr 未分流:stdout=%q stderr=%q", res.Stdout, res.Stderr)
		}
		if res.Truncated {
			t.Fatal("小输出不该标记截断")
		}
	})

	t.Run("多行 set -e 脚本作为单个 argv 参数送达", func(t *testing.T) {
		res := run(t, "set -e\necho one\necho 'two three'\necho four")
		if res.ExitCode != 0 {
			t.Fatalf("退出码 = %d,want 0(stderr=%q)", res.ExitCode, res.Stderr)
		}
		out := res.Stdout
		for _, want := range []string{"one", "two three", "four"} {
			if !strings.Contains(out, want) {
				t.Fatalf("输出缺少 %q:%q", want, out)
			}
		}
		if strings.Index(out, "one") > strings.Index(out, "two three") {
			t.Fatalf("多行脚本执行顺序不符:%q", out)
		}
	})

	t.Run("注入防护:元字符当字面量,绝不被远端二次解释", func(t *testing.T) {
		marker := "/tmp/pw-e2e-injected-" + fmt.Sprint(os.Getpid())
		_, _ = c.Exec(t, "rm", "-f", marker)
		res := sshDialerRunArgs(t, addr, cfg, []string{"echo", "; rm -rf /", "$(touch " + marker + ")"})
		if !strings.Contains(res.Stdout, "; rm -rf /") || !strings.Contains(res.Stdout, marker) {
			t.Fatalf("元字符参数未被当字面量回显:%q", res.Stdout)
		}
		if _, err := c.Exec(t, "test", "-e", marker); err == nil {
			t.Fatalf("参数里的 $(touch ...) 竟被远端执行(注入!)")
		}
	})

	t.Run("输出超过单流上限:截断标记 + 会话不中断", func(t *testing.T) {
		// 远端吐 上限 + 2MiB 的 x:只应回传上限长度,且**不得**因短写被判成会话错误。
		oversize := maxExecOutputBytes + (2 << 20)
		res := run(t, fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x", oversize))
		if res.ExitCode != 0 {
			t.Fatalf("退出码 = %d,want 0(超限输出不该影响命令成败)", res.ExitCode)
		}
		if !res.Truncated {
			t.Fatal("超过单流上限应标记 Truncated")
		}
		if len(res.Stdout) != maxExecOutputBytes {
			t.Fatalf("stdout 长度 = %d,want %d(截到上限)", len(res.Stdout), maxExecOutputBytes)
		}
		if strings.Trim(res.Stdout, "x") != "" {
			t.Fatal("截断内容应是远端输出的前缀")
		}
	})
}

// sshDialerRunArgs 直接用 argv 形式跑一条真命令(不经 sh:验「参数即字面量」)。
func sshDialerRunArgs(t *testing.T, addr string, cfg SSHConfig, args []string) *ExecResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := sshDialer{}.Run(ctx, addr, cfg, args)
	if err != nil {
		t.Fatalf("真 Exec(%q) 失败: %v", args, err)
	}
	return res
}
