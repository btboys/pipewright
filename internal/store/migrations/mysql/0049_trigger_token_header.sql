-- 0049 trigger_token_header(MySQL):pipeline_triggers 增自定义 token 校验请求头列。
-- 空串(默认)= 按内置回退链自动识别;非空 = 只校验该请求头。非敏感。
ALTER TABLE pipeline_triggers ADD COLUMN token_header VARCHAR(64) NOT NULL DEFAULT '';
