-- 触发提交的说明信息(push 事件的 commit message 首行),供流水线通知引用展示。
-- webhook 接收时从 commits[].message / head_commit.message 解析;手动/定时/串联触发为空串。
ALTER TABLE pipeline_runs ADD COLUMN trigger_commit_message VARCHAR(512) NOT NULL DEFAULT '';
