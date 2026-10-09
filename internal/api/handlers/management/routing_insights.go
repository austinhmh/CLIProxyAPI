package management

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// GetRoutingInsights reports sticky-session activity and SDK hash-score previews.
func (handler *Handler) GetRoutingInsights(context *gin.Context) {
	if handler == nil || handler.authManager == nil {
		context.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}

	window := 5 * time.Minute
	if rawWindow := strings.TrimSpace(context.Query("window")); rawWindow != "" {
		if parsedWindow, errParse := time.ParseDuration(rawWindow); errParse == nil && parsedWindow > 0 {
			window = parsedWindow
		}
	}
	provider := strings.TrimSpace(context.Query("provider"))
	model := strings.TrimSpace(context.Query("model"))
	requestKey := strings.TrimSpace(context.Query("idempotency_key"))
	sessions := handler.authManager.SessionAffinitySnapshot(window)
	hashPreview := handler.authManager.BalancedHashPreview(provider, model, requestKey)

	authCounts := make(map[string]int, len(sessions))
	for _, session := range sessions {
		if authID := strings.TrimSpace(session.AuthID); authID != "" {
			authCounts[authID]++
		}
	}
	countItems := make([]gin.H, 0, len(authCounts))
	countValues := make([]int, 0, len(authCounts))
	for authID, count := range authCounts {
		countItems = append(countItems, gin.H{"auth_id": authID, "active_sessions": count})
		countValues = append(countValues, count)
	}
	sort.Slice(countItems, func(leftIndex, rightIndex int) bool {
		leftCount := countItems[leftIndex]["active_sessions"].(int)
		rightCount := countItems[rightIndex]["active_sessions"].(int)
		if leftCount == rightCount {
			return countItems[leftIndex]["auth_id"].(string) < countItems[rightIndex]["auth_id"].(string)
		}
		return leftCount > rightCount
	})

	context.JSON(http.StatusOK, gin.H{
		"window_seconds":             int(window.Seconds()),
		"active_session_bindings":    sessions,
		"active_session_auth_counts": countItems,
		"balance_metrics":            computeBalanceMetrics(countValues),
		"hash_preview":               hashPreview,
		"provider":                   provider,
		"model":                      model,
	})
}

func computeBalanceMetrics(counts []int) gin.H {
	if len(counts) == 0 {
		return gin.H{
			"active_auths":          0,
			"total_active_sessions": 0,
			"max_min_ratio":         0.0,
			"top1_share":            0.0,
			"gini":                  0.0,
		}
	}

	total := 0
	maximum := counts[0]
	minimum := counts[0]
	for _, count := range counts {
		total += count
		if count > maximum {
			maximum = count
		}
		if count < minimum {
			minimum = count
		}
	}
	maxMinRatio := 0.0
	if minimum > 0 {
		maxMinRatio = float64(maximum) / float64(minimum)
	}
	topShare := float64(maximum) / float64(total)
	differenceSum := 0.0
	for _, leftCount := range counts {
		for _, rightCount := range counts {
			differenceSum += math.Abs(float64(leftCount - rightCount))
		}
	}
	return gin.H{
		"active_auths":          len(counts),
		"total_active_sessions": total,
		"max_min_ratio":         maxMinRatio,
		"top1_share":            topShare,
		"gini":                  differenceSum / (2 * float64(len(counts)) * float64(total)),
	}
}

// GetSessionMonitorPage serves a read-only interface without injecting upstream data into HTML.
func (handler *Handler) GetSessionMonitorPage(context *gin.Context) {
	if handler == nil {
		context.AbortWithStatus(http.StatusNotFound)
		return
	}
	context.Header("Cache-Control", "no-store")
	context.Data(http.StatusOK, "text/html; charset=utf-8", []byte(routingInsightsMonitorHTML))
}

const routingInsightsMonitorHTML = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>CLIProxy 路由监控</title>
  <style>
    body { font-family: sans-serif; margin: 2rem; background: #161923; color: #e5e7eb; }
    input, button { margin: .3rem; padding: .5rem; background: #252b37; color: inherit; border: 1px solid #515b6d; }
    table { border-collapse: collapse; width: 100%; margin: 1rem 0; }
    th, td { text-align: left; border-bottom: 1px solid #515b6d; padding: .4rem; }
    small { color: #acb4c2; }
  </style>
</head>
<body>
  <h1>CLIProxy 路由与哈希评分</h1>
  <p><small>这里只预览评分；实际请求仍沿用原有路由策略。</small></p>
  <input id="key" type="password" placeholder="管理密钥">
  <input id="model" placeholder="模型名">
  <input id="provider" placeholder="提供商，可留空">
  <button id="refresh">刷新</button>
  <p id="status"></p>
  <h2>候选凭据评分</h2>
  <table id="scores"><thead><tr><th>凭据</th><th>提供商</th><th>总分</th><th>是否受限</th></tr></thead><tbody></tbody></table>
  <h2>活跃会话绑定</h2>
  <table id="sessions"><thead><tr><th>会话</th><th>凭据</th><th>模型</th></tr></thead><tbody></tbody></table>
  <script>
    const getElement = (identifier) => document.getElementById(identifier);
    function appendRows(tableIdentifier, entries, fields) {
      const body = getElement(tableIdentifier).querySelector("tbody");
      body.replaceChildren();
      for (const entry of entries) {
        const row = document.createElement("tr");
        for (const field of fields) {
          const cell = document.createElement("td");
          cell.textContent = String(entry[field] ?? "");
          row.appendChild(cell);
        }
        body.appendChild(row);
      }
    }
    async function refresh() {
      const parameters = new URLSearchParams({
        window: "5m",
        model: getElement("model").value.trim(),
        provider: getElement("provider").value.trim(),
        idempotency_key: String(Date.now()),
      });
      const key = getElement("key").value.trim();
      const headers = key ? { Authorization: "Bearer " + key } : {};
      try {
        const response = await fetch("/v0/management/routing-insights?" + parameters, { headers });
        if (!response.ok) throw new Error("HTTP " + response.status);
        const data = await response.json();
        appendRows("scores", data.hash_preview || [], ["auth_id", "provider", "total_score", "blocked"]);
        appendRows("sessions", data.active_session_bindings || [], ["session_id", "auth_id", "model_key"]);
        getElement("status").textContent = "已更新：" + new Date().toLocaleTimeString();
      } catch (error) {
        getElement("status").textContent = "加载失败：" + String(error);
      }
    }
    getElement("refresh").addEventListener("click", refresh);
  </script>
</body>
</html>`
