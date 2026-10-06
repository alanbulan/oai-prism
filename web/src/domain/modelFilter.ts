/**
 * 模型清单工具 —— 后端 /v1/models 的模型名、展示名与推理档位都来自上游在售清单
 * （网关定时拉取 Prism 的 /api/inference/models 与网页的推理强度选项），前端零硬编码：
 *   1. 主条目 = 名字就是上游模型 id 的条目（upstream_model === id）；"<id>-<档位>" 变体与自定义别名不算
 *   2. 各模型的可用档位与默认档位 = 主条目的 reasoning_efforts / default_reasoning_effort
 *   3. 默认模型 = 带 default 标记的条目对应的上游模型，没有就取第一个主条目（上游的顺序）
 */

/** /v1/models 的条目（含网关扩展字段） */
export interface ModelEntry {
  id: string;
  name?: string;
  upstream_model?: string;
  reasoning_effort?: string;
  reasoning_efforts?: string[];
  default_reasoning_effort?: string;
  default?: boolean;
}

/** 主模型条目（按后端给的顺序，即上游的顺序）。 */
export const pickMainModels = <T extends ModelEntry>(list: T[]): T[] => list.filter((m) => m.id === m.upstream_model);

/** 默认模型：带 default 标记的条目对应的上游模型；没有标记就取第一个主条目。 */
export const defaultMainModel = (list: ModelEntry[]): string | undefined =>
  list.find((m) => m.default)?.upstream_model ?? pickMainModels(list)[0]?.id;
