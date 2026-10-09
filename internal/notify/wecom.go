package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/btboys/pipewright/internal/i18n"
)

// 企业微信群机器人投递。
//
// 配置 = 群机器人 webhook 地址(复用 ChannelConfig.URL),群机器人本身不需签名密钥
// (鉴权靠 URL 内置的 key 参数)。报文形状为企微 markdown:
//
//	{"msgtype":"markdown","markdown":{"content":"<渲染后的 markdown 正文>"}}
//
// 成功判定:与飞书同理,企微即便参数错误也常回 HTTP 200,但 body 内 errcode!=0。
// 必须解析返回 JSON 的 errcode(0=成功,非 0=失败,人读 errmsg 透出),否则会误报成功、群里收不到。
// SSRF 收口与通用 webhook 一致(投递前再校验一次 URL,CheckRedirect 每跳复校)。

// wecomMarkdownBody 是企微群机器人 markdown 消息体。
type wecomMarkdownBody struct {
	MsgType  string           `json:"msgtype"` // 恒 "markdown"
	Markdown wecomMarkdownObj `json:"markdown"`
}

type wecomMarkdownObj struct {
	Content string `json:"content"`
}

// wecomResp 是企微机器人返回体(只取判定/人读所需字段)。
type wecomResp struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// sendWecom 经注入的 *http.Client POST markdown 消息到企微群机器人 webhook。
//   - SSRF 收口:投递前再校验一次 URL(与保存时一致;CheckRedirect 每跳复校)。
//   - 超时:注入 client.Timeout 兜底 + 另套 8s context。
//   - 成功判定:HTTP 2xx **且** 返回 JSON errcode==0(企微参数错也回 200,必须看 errcode)。
//   - 群机器人无需签名,故不取用 sealed(无敏感字段)。
func (s *service) sendWecom(ctx context.Context, ch *Channel, payload Payload) error {
	target := ch.Config.URL
	if !validWebhookURL(target) {
		return fmt.Errorf("企业微信 webhook 地址不被允许:仅 http/https,且不可指向云元数据/链路本地地址")
	}

	body := wecomMarkdownBody{
		MsgType:  "markdown",
		Markdown: wecomMarkdownObj{Content: renderMarkdownBody(payload, flavorWecom)},
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("企业微信载荷序列化失败")
	}

	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("企业微信请求构造失败:地址可能非法")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Pipewright-notify/1")

	resp, err := s.client.Do(req)
	if err != nil {
		return mapWebhookTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxWebhookRespBytes))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("企业微信接收端返回非 2xx 状态:HTTP %d", resp.StatusCode)
	}

	// 企微参数错误也回 HTTP 200,必须解析 errcode 才能判定真成功(否则误报成功、群里收不到)。
	var wr wecomResp
	if err := json.Unmarshal(respBody, &wr); err != nil {
		return fmt.Errorf("企业微信返回体无法解析,投递结果未知")
	}
	if wr.ErrCode != 0 {
		msg := strings.TrimSpace(wr.ErrMsg)
		if msg == "" {
			return fmt.Errorf("企业微信机器人拒收:errcode=%d", wr.ErrCode)
		}
		return fmt.Errorf("企业微信机器人拒收:errcode=%d %s", wr.ErrCode, msg)
	}
	return nil
}

// ─── 通用 markdown 正文渲染(企微 / 钉钉共用) ───────────────────────────────────
//
// 两种渲染形态:
//   - **流水线卡片**:载荷带 fieldPipeline 键(平台默认渲染的**运行终态事件**)时,按
//     「Pipewright 流水线消息通知」卡片渲染(对齐 Flow 通知格式):彩色标题 + 流水线名(可点击)
//   - 环境/执行人/触发信息/阶段/任务/运行状态 + 提交说明引用行。空值行省略。
//   - **旧版字段列表**:其余载荷(自定义模板 / 审批 / 异常 / 测试通知)保持
//     「# 标题 + 正文 + **标签**:值 字段列表」不变。
//
// markdownFlavor 区分企微与钉钉:企微 markdown 支持 <font color="info">(绿) 标题,
// 钉钉不支持(会原样显示标签),故彩色标题仅企微 flavor 输出。
type markdownFlavor int

const (
	flavorWecom markdownFlavor = iota
	flavorDingtalk
)

// renderMarkdownBody 把 Payload 渲染为群机器人 markdown 正文。
func renderMarkdownBody(p Payload, flavor markdownFlavor) string {
	if strings.TrimSpace(p.Fields[fieldPipeline]) != "" {
		if card := renderPipelineCard(p, flavor); card != "" {
			return card
		}
	}

	var b strings.Builder
	title := strings.TrimSpace(p.Title)
	if title == "" {
		title = i18n.T(p.Lang, "Pipewright 通知")
	}
	b.WriteString("# ")
	b.WriteString(title)
	b.WriteString("\n")

	if body := strings.TrimSpace(p.Body); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteString("\n")
	}

	if fields := orderedFieldKeys(p.Fields); len(fields) > 0 {
		b.WriteString("\n")
		for _, k := range fields {
			b.WriteString("**")
			b.WriteString(feishuFieldLabel(k, p.Lang))
			b.WriteString("**:")
			b.WriteString(p.Fields[k])
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderPipelineCard 渲染「Pipewright 流水线消息通知」卡片(运行终态事件):
//
//	# <绿标题:Pipewright 流水线消息通知>          ← 仅企微 flavor 输出彩色标签
//	**流水线**: [名称](运行详情链接)
//	**流水线环境**: 正式环境
//	**执行人**: admin
//	**触发信息**: 流水线定时自动触发
//	**流水线阶段**: 部署
//	**流水线任务**: 纷析云部署
//	**运行状态**: ✅运行成功
//
//	> chore(agent): 端点生成器改为显式任务并支持仓库级排除清单
//
// 空值行省略;提交说明取首行(存字段时已截断)。pipelineUrl 缺失时流水线名为纯文本。
func renderPipelineCard(p Payload, flavor markdownFlavor) string {
	f := p.Fields
	var b strings.Builder
	title := i18n.T(p.Lang, "Pipewright 流水线消息通知")
	if flavor == flavorWecom {
		b.WriteString("# <font color=\"info\">")
		b.WriteString(title)
		b.WriteString("</font>")
	} else {
		b.WriteString("# ")
		b.WriteString(title)
	}

	// 流水线:优先渲染为可点击链接(运行详情页)。
	b.WriteString("\n\n**")
	b.WriteString(i18n.T(p.Lang, "流水线"))
	b.WriteString("**:")
	if u := strings.TrimSpace(f[fieldPipelineURL]); u != "" {
		b.WriteString("[")
		b.WriteString(f[fieldPipeline])
		b.WriteString("](")
		b.WriteString(u)
		b.WriteString(")")
	} else {
		b.WriteString(f[fieldPipeline])
	}

	line := func(key, label, value string) {
		if v := strings.TrimSpace(value); v != "" {
			b.WriteString("\n**")
			b.WriteString(i18n.T(p.Lang, label))
			b.WriteString("**:")
			b.WriteString(v)
		}
	}
	line("environment", "流水线环境", f["environment"])
	line("actor", "执行人", f["actor"])
	line("triggerType", "触发信息", triggerInfoLabel(f["triggerType"], p.Lang))
	line("stage", "流水线阶段", f["stage"])
	line("task", "流水线任务", f["task"])

	// 运行状态:事件图标 + 本地化事件标签。
	event := strings.TrimSpace(f["event"])
	b.WriteString("\n**")
	b.WriteString(i18n.T(p.Lang, "运行状态"))
	b.WriteString("**:")
	b.WriteString(eventStatusIcon(event))
	b.WriteString(eventLabel(event, p.Lang))

	// 提交说明引用行(> 首行)。
	if msg := strings.TrimSpace(f["commitMessage"]); msg != "" {
		b.WriteString("\n\n> ")
		b.WriteString(msg)
	}
	return b.String()
}

// eventStatusIcon 返回事件对应的运行状态图标(✅ 成功 / ❌ 失败 / 🔄 回滚)。
func eventStatusIcon(event string) string {
	switch event {
	case EventBuildSucceeded, EventDeploySucceeded:
		return "✅"
	case EventBuildFailed, EventDeployFailed, EventHealthCheckFailed:
		return "❌"
	case EventRollback:
		return "🔄"
	default:
		return "🔔"
	}
}

// triggerInfoLabel 把触发类型枚举渲染为本地化人读文案(空/未知原样透出)。
func triggerInfoLabel(triggerType, lang string) string {
	switch strings.TrimSpace(triggerType) {
	case "schedule":
		return i18n.T(lang, "流水线定时自动触发")
	case "webhook":
		return i18n.T(lang, "代码推送触发")
	case "manual":
		return i18n.T(lang, "手动触发")
	case "chain":
		return i18n.T(lang, "上游运行串联触发")
	default:
		return strings.TrimSpace(triggerType)
	}
}

// orderedFieldKeys 返回字段 key 的稳定展示顺序(业务顺序优先,复用 feishuFieldOrder;其余字母序)。
func orderedFieldKeys(fields map[string]string) []string {
	if len(fields) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(fields))
	keys := make([]string, 0, len(fields))
	for _, k := range feishuFieldOrder {
		if _, ok := fields[k]; ok {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	rest := make([]string, 0)
	for k := range fields {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}
