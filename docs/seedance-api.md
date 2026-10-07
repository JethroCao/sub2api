# Seedance 原生 API

支持火山方舟 Ark 的异步视频任务协议，无需把 `content[]` 转换成 OpenAI `messages` 或 Grok `prompt`。

## 配置

1. 创建 OpenAI 平台的 **API Key** 账号，填写 Ark API Key，Base URL 使用 `https://ark.cn-beijing.volces.com/api/v3`。兼容服务可填写自己的 `/api/v3` 或 `/v3` Base URL。
2. 在账号的端点能力中勾选 **Seedance (Ark)**。默认不启用，避免请求误调度到其他 OpenAI 账号。支持创建、编辑与批量编辑。
3. 将账号加入 OpenAI 分组并启用分组的「允许图片生成」媒体权限；合成分组也可路由到这些账号。
4. 配置模型映射，例如将公开模型名 `seedance-video` 映射到实际 `doubao-seedance-*` 模型或 `ep-*` 推理接入点。配置对应模型的输出 token 价格；本接口不使用 Grok 的按秒视频价格。

## 调用

```bash
curl "$SUB2API_BASE_URL/api/v3/contents/generations/tasks" \
  -H "Authorization: Bearer $SUB2API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "seedance-video",
    "content": [{"type": "text", "text": "海浪轻轻拍打沙滩"}],
    "duration": 5,
    "resolution": "720p",
    "ratio": "16:9",
    "generate_audio": true
  }'

# 使用创建响应中的原生 id 查询，直至 succeeded / failed / cancelled 等终态。
curl "$SUB2API_BASE_URL/api/v3/contents/generations/tasks/$TASK_ID" \
  -H "Authorization: Bearer $SUB2API_KEY"

curl -X DELETE "$SUB2API_BASE_URL/api/v3/contents/generations/tasks/$TASK_ID" \
  -H "Authorization: Bearer $SUB2API_KEY"
```

亦支持 `/v3`、`/v1` 和无版本前缀别名。Ark SDK 的 Base URL 可改为 `$SUB2API_BASE_URL/api/v3`。文本、图片、视频、音频内容、角色及扩展参数原样传递，仅按账号配置改写模型名；响应保持上游原生格式。

## 任务与计费

- 查询和删除只能访问同一用户、同一 API Key、同一分组创建的任务，并始终使用原提交账号；不会转到其他账号查询。
- 创建时不扣 token 用量。首次查询到 `succeeded` 后，根据上游 `usage.completion_tokens` 计费；重复查询由共享缓存声明和持久化用量去重共同保护。失败、排队及运行中的任务不计费。
- Redis 保存任务绑定及创建时的模型快照 8 天，覆盖 Ark 最长 72 小时任务和 7 天样片有效期。需保留 Redis 状态并在有效期内查询完成结果。当前不会后台轮询；只使用回调而不查询的任务不会自动结算。
- 不开放上游的任务列表接口，防止共享账号的任务泄露给其他用户。删除遵循上游语义，不自动退款。
- 异步创建的上游错误不自动重试，以免重复创建付费任务。

协议依据：[火山官方 Go SDK](https://github.com/volcengine/volcengine-go-sdk/blob/master/service/arkruntime/model/content_generation.go)、[创建任务文档](https://www.volcengine.com/docs/82379/1520757)、[查询任务文档](https://www.volcengine.com/docs/82379/1521309)。

## Seedance 2.0 / 2.5 官方分档计费

OpenAI API Key 账号可在 `credentials` 中显式配置下面两个字符串字段，启用火山官方刊例价；未启用的账号继续使用原来的分组/渠道 token 定价。

```json
{
  "seedance_official_pricing": "true",
  "seedance_usd_to_cny_rate": "6.7351"
}
```

汇率是 **1 美元对应的人民币金额**。上例取 2026-10-07 [中国银行中行折算价](https://www.boc.cn/sourcedb/whpj/)；它是可调整的固定配置，不是每天自动刷新的汇率。不要把人民币单价直接填进美元账本。每个任务创建时保存完整价格档位和汇率快照，后续调整不会追溯修改旧任务。

[火山官方价格](https://docs.volcengine.com/docs/ark/model-pricing)（2026-10-07 核对，人民币 / 百万 `completion_tokens`）：

| 模型 | 输出分辨率 | 不含输入视频 | 含输入视频 |
| --- | --- | ---: | ---: |
| Seedance 2.0 | 480p / 720p | 46 | 28 |
| Seedance 2.0 | 1080p | 51 | 31 |
| Seedance 2.0 | 4k | 26 | 16 |
| Seedance 2.5 | 480p / 720p | 70 | 42 |
| Seedance 2.5 | 1080p | 77 | 46 |

- 美元费用 = 官方实际 `usage.completion_tokens` × 对应人民币单价 ÷ 1,000,000 ÷ 汇率；再应用基础分组/用户倍率。倍率 1 即不加价，不叠加文本 token 高峰倍率，也不使用 Grok 按秒价。
- 公开模型名支持 `seedance-2.0` / `seedance-2.5` 及官方 `doubao-seedance-2-0-*` / `doubao-seedance-2-5-*` 名称。映射到 `ep-*` 时按公开名识别模型版本；管理员须确保接入点对应正确版本。fast / mini 和其他版本不套用本表。
- 输入中有 `content[].type=video_url` 才用含视频价；图片和音频不会被当成视频。分辨率优先使用成功查询响应的 `resolution`；缺省按请求参数（含 `--rs` 文本参数）及官方默认 720p。
- 2.5 Draft 按 480p；正式视频的 `content[].type=draft_task` 引用同一用户、API Key、分组创建的样片，调度固定到原提交账号，默认且仅支持 1080p，按样片原始请求是否含视频决定价格。两步各自按官方实际 token 独立结算，样片产物本身不算输入视频。
- 缺少汇率、没有对应价格或无法验证样片归属时，在创建付费上游任务之前拒绝请求，不静默生成免费任务。本模式覆盖上述两种模型的在线 token 刊例价，不代替上游合同折扣或其他增值服务收费。
