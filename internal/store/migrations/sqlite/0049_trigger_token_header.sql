-- 0049 trigger_token_header:自定义 token 校验请求头。
-- 空串(默认)= 按内置回退链自动识别 X-Gitee-Token → X-Codeup-Token → X-Gitlab-Token;
-- 非空 = 只校验该请求头(用于平台/网关自定义头名)。非敏感,不加密。
ALTER TABLE pipeline_triggers ADD COLUMN token_header TEXT NOT NULL DEFAULT '';
