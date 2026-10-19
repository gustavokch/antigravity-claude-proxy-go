/**
 * Dashboard Charts Module
 * 职责：使用 Chart.js 渲染配额分布图和使用趋势图
 *
 * 调用时机：
 *   - dashboard 组件 init() 时初始化图表
 *   - 筛选器变化时更新图表数据
 *   - $store.data 更新时刷新图表
 *
 * 图表类型：
 *   1. Quota Distribution（饼图）：按模型家族或具体模型显示配额分布
 *   2. Usage Trend（折线图）：显示历史使用趋势
 *
 * 特殊处理：
 *   - 使用 _trendChartUpdateLock 防止并发更新导致的竞争条件
 *   - 通过 debounce 优化频繁更新的性能
 *   - 响应式处理：移动端自动调整图表大小和标签显示
 *
 * @module DashboardCharts
 */
window.DashboardCharts = window.DashboardCharts || {};

// Helper to get CSS variable values (alias to window.utils.getThemeColor)
const getThemeColor = (name) => window.utils.getThemeColor(name);

// Chart series come from the Signal Box palette (--sb-chart-1..8). They are
// read at each rebuild so a theme change picks up the new values.
const SERIES_COUNT = 8;
const seriesColor = (index) =>
  getThemeColor(`--sb-chart-${(index % SERIES_COUNT) + 1}`);

// Chart chrome (grid, ticks, tooltip) from the same theme, read per rebuild.
const chartTheme = () => ({
  panel: getThemeColor("--sb-panel"),
  ink: getThemeColor("--sb-ink"),
  ink2: getThemeColor("--sb-ink-2"),
  grid: getThemeColor("--sb-chart-grid"),
  tick: getThemeColor("--sb-chart-tick"),
  font: "Barlow, system-ui, sans-serif",
});

// One trace ink on the trend chart; families are split by dash pattern
// (solid = claude, dashed = gemini, dotted = other), never by colour.
const TRACE_COLOR = () => seriesColor(0);
const FAMILY_DASH = {
  claude: [],
  gemini: [6, 4],
  other: [2, 3],
};

// Export constants for filter module
window.DashboardConstants = { seriesColor, TRACE_COLOR, FAMILY_DASH };

// Module-level lock to prevent concurrent chart updates (fixes race condition)
let _trendChartUpdateLock = false;

/**
 * Check if canvas is ready for Chart creation
 * @param {HTMLCanvasElement} canvas - Canvas element
 * @returns {boolean} True if canvas is ready
 */
function isCanvasReady(canvas) {
  if (!canvas || !canvas.isConnected) return false;
  if (canvas.offsetWidth === 0 || canvas.offsetHeight === 0) return false;

  try {
    const ctx = canvas.getContext("2d");
    return !!ctx;
  } catch (e) {
    return false;
  }
}

/**
 * Create a Chart.js line dataset. Flat strokes: no fill, no gradient.
 * @param {string} label - Dataset label
 * @param {Array} data - Data points
 * @param {string} color - Series colour (a --sb-chart-* value)
 * @param {Array} [dash] - borderDash pattern (empty = solid)
 * @returns {object} Chart.js dataset configuration
 */
window.DashboardCharts.createDataset = function (label, data, color, dash) {
  return {
    label,
    data,
    borderColor: color,
    backgroundColor: color,
    borderWidth: 1.5,
    borderDash: dash || [],
    tension: 0.35,
    fill: false,
    pointRadius: 2,
    pointHoverRadius: 5,
    pointBackgroundColor: color,
    pointBorderColor: chartTheme().panel,
    pointBorderWidth: 1,
  };
};

/**
 * Recompute the pool-wide quota figure and the per-family quota tracks.
 * No canvas involved. Sets component.stats.overallHealth and
 * component.familyQuota (families with reported quota only, lowest first).
 * @param {object} component - Dashboard component instance
 */
window.DashboardCharts.updateQuotaSummary = function (component) {
  // Use UNFILTERED data for global health
  const rows = Alpine.store("data").getUnfilteredQuotaData();
  if (!rows || rows.length === 0) {
    component.stats.overallHealth = 0;
    component.familyQuota = [];
    return;
  }

  const accounts = Alpine.store("data").accounts || [];
  const byFamily = {};
  let totalHealthSum = 0;
  let totalModelCount = 0;

  rows.forEach((row) => {
    const quotaInfo = row.quotaInfo || [];
    // Null pct = unknown quota: excluded from averages so unknowns never
    // drag health down with a false 0%.
    const known = quotaInfo.filter((q) => q && q.pct !== null && q.pct !== undefined);
    if (quotaInfo.length > 0 && known.length === 0) return; // all-unknown row

    // No account reports this model (empty quotaInfo): counts as 0%.
    const avgHealth = known.length > 0
      ? known.reduce((sum, q) => sum + q.pct, 0) / known.length
      : 0;
    totalHealthSum += avgHealth;
    totalModelCount++;

    if (known.length === 0) return;
    const family = row.family || "unknown";
    if (!byFamily[family]) {
      byFamily[family] = { sum: 0, n: 0, resetAt: null };
    }
    const agg = byFamily[family];
    agg.sum += avgHealth;
    agg.n++;

    // Next reset: earliest resetTime among this model's windows that are not full.
    accounts.forEach((acc) => {
      if (acc.enabled === false) return;
      const limit = acc.limits?.[row.modelId];
      if (!limit || !limit.resetTime) return;
      if (limit.remainingFraction === null || limit.remainingFraction === undefined) return;
      if (limit.remainingFraction >= 1) return;
      if (!agg.resetAt || new Date(limit.resetTime) < new Date(agg.resetAt)) {
        agg.resetAt = limit.resetTime;
      }
    });
  });

  component.stats.overallHealth = totalModelCount > 0
    ? Math.round(totalHealthSum / totalModelCount)
    : 0;

  component.familyQuota = Object.entries(byFamily)
    .map(([family, agg]) => ({
      family,
      pct: Math.round(agg.sum / agg.n),
      resetAt: agg.resetAt,
    }))
    .sort((a, b) => a.pct - b.pct);
};

/**
 * Update usage trend line chart
 * @param {object} component - Dashboard component instance
 */
window.DashboardCharts.updateTrendChart = function (component) {
  // Prevent concurrent updates (fixes race condition on rapid toggling)
  if (_trendChartUpdateLock) {
    if (window.UILogger) window.UILogger.debug("[updateTrendChart] Update already in progress, skipping");
    return;
  }
  _trendChartUpdateLock = true;

  const logger = window.UILogger || console;
  logger.debug("[updateTrendChart] Starting update...");

  const canvas = document.getElementById("usageTrendChart");
  
  // FORCE DESTROY: Check for existing chart on the canvas element property
  if (canvas) {
      if (canvas._chartInstance) {
        console.debug("Destroying existing trend chart from canvas property");
        try {
            canvas._chartInstance.stop();
            canvas._chartInstance.destroy();
        } catch(e) { if (window.UILogger) window.UILogger.debug(e); }
        canvas._chartInstance = null;
      }
      
      // Also try Chart.js registry
      if (typeof Chart !== "undefined" && Chart.getChart) {
          const regChart = Chart.getChart(canvas);
          if (regChart) {
              try { regChart.stop(); regChart.destroy(); } catch(e) {}
          }
      }
  }

  // Also check component state
  if (component.charts.usageTrend) {
    try {
      component.charts.usageTrend.stop();
      component.charts.usageTrend.destroy();
    } catch (e) { }
    component.charts.usageTrend = null;
  }

  // Safety checks
  if (!canvas) {
    if (window.UILogger) window.UILogger.debug("[updateTrendChart] Canvas not found in DOM");
    _trendChartUpdateLock = false;
    return;
  }
  if (typeof Chart === "undefined") {
    if (window.UILogger) window.UILogger.warn("[updateTrendChart] Chart.js not loaded");
    _trendChartUpdateLock = false;
    return;
  }

  if (window.UILogger) window.UILogger.debug("[updateTrendChart] Canvas element:", {
    exists: !!canvas,
    isConnected: canvas.isConnected,
    width: canvas.offsetWidth,
    height: canvas.offsetHeight,
    parentElement: canvas.parentElement?.tagName,
  });

  if (!isCanvasReady(canvas)) {
    if (window.UILogger) window.UILogger.debug("[updateTrendChart] Canvas not ready", {
      isConnected: canvas.isConnected,
      width: canvas.offsetWidth,
      height: canvas.offsetHeight,
    });
    _trendChartUpdateLock = false;
    return;
  }

  // Clear canvas to ensure clean state after destroy
  try {
    const ctx = canvas.getContext("2d");
    if (ctx) {
      ctx.clearRect(0, 0, canvas.width, canvas.height);
    }
  } catch (e) {
    if (window.UILogger) window.UILogger.debug("[updateTrendChart] Failed to clear canvas:", e.message);
  }

  if (window.UILogger) window.UILogger.debug(
    "[updateTrendChart] Canvas is ready, proceeding with chart creation"
  );

  // Use filtered history data based on time range
  const history = window.DashboardFilters.getFilteredHistoryData(component);
  if (!history || Object.keys(history).length === 0) {
    if (window.UILogger) window.UILogger.debug("No history data available for trend chart (after filtering)");
    component.hasFilteredTrendData = false;
    _trendChartUpdateLock = false;
    return;
  }

  component.hasFilteredTrendData = true;

  // Sort entries by timestamp for correct order
  const sortedEntries = Object.entries(history).sort(
    ([a], [b]) => new Date(a).getTime() - new Date(b).getTime()
  );

  // Determine if data spans multiple days (for smart label formatting)
  const timestamps = sortedEntries.map(([iso]) => new Date(iso));
  const isMultiDay = timestamps.length > 1 &&
    timestamps[0].toDateString() !== timestamps[timestamps.length - 1].toDateString();

  // Helper to format X-axis labels based on time range and multi-day status
  const formatLabel = (date) => {
    const timeRange = component.timeRange || '24h';

    if (timeRange === '7d') {
      // Week view: show MM/DD
      return date.toLocaleDateString([], { month: '2-digit', day: '2-digit' });
    } else if (isMultiDay || timeRange === 'all') {
      // Multi-day data: show MM/DD HH:MM
      return date.toLocaleDateString([], { month: '2-digit', day: '2-digit' }) + ' ' +
             date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    } else {
      // Same day: show HH:MM only
      return date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    }
  };

  const labels = [];
  const datasets = [];

  if (component.displayMode === "family") {
    // Aggregate by family
    const dataByFamily = {};
    component.selectedFamilies.forEach((family) => {
      dataByFamily[family] = [];
    });

    sortedEntries.forEach(([iso, hourData]) => {
      const date = new Date(iso);
      labels.push(formatLabel(date));

      component.selectedFamilies.forEach((family) => {
        const familyData = hourData[family];
        const count = familyData?._subtotal || 0;
        dataByFamily[family].push(count);
      });
    });

    // Build datasets for families — single trace hue, dash pattern per family
    component.selectedFamilies.forEach((family) => {
      const familyKey =
        "family" + family.charAt(0).toUpperCase() + family.slice(1);
      const label = Alpine.store("global").t(familyKey);
      datasets.push(
        window.DashboardCharts.createDataset(
          label,
          dataByFamily[family],
          TRACE_COLOR(),
          FAMILY_DASH[family] || FAMILY_DASH.other
        )
      );
    });
  } else {
    // Show individual models
    const dataByModel = {};

    // Initialize data arrays
    component.families.forEach((family) => {
      (component.selectedModels[family] || []).forEach((model) => {
        const key = `${family}:${model}`;
        dataByModel[key] = [];
      });
    });

    sortedEntries.forEach(([iso, hourData]) => {
      const date = new Date(iso);
      labels.push(formatLabel(date));

      component.families.forEach((family) => {
        const familyData = hourData[family] || {};
        (component.selectedModels[family] || []).forEach((model) => {
          const key = `${family}:${model}`;
          const val = familyData[model];
          const count = typeof val === 'object' && val !== null ? (val.requests || 0) : (val || 0);
          dataByModel[key].push(count);
        });
      });
    });

    // Build datasets for models
    component.families.forEach((family) => {
      (component.selectedModels[family] || []).forEach((model, modelIndex) => {
        const key = `${family}:${model}`;
        const color = window.DashboardFilters.getModelColor(family, modelIndex);
        datasets.push(
          window.DashboardCharts.createDataset(
            model,
            dataByModel[key],
            color,
            FAMILY_DASH[family] || FAMILY_DASH.other
          )
        );
      });
    });
  }

  const theme = chartTheme();
  try {
    const newChart = new Chart(canvas, {
      type: "line",
      data: { labels, datasets },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        animation: {
          duration: 300, // Reduced animation for faster updates
        },
        interaction: {
          mode: "index",
          intersect: false,
        },
        plugins: {
          legend: { display: false },
          tooltip: {
            backgroundColor: theme.panel,
            titleColor: theme.ink,
            bodyColor: theme.ink2,
            borderColor: theme.ink,
            borderWidth: 1,
            cornerRadius: 0,
            padding: 10,
            displayColors: true,
            titleFont: { family: theme.font, size: 11, weight: "600" },
            bodyFont: { family: theme.font, size: 11 },
            callbacks: {
              label: function (context) {
                return context.dataset.label + ": " + context.parsed.y;
              },
            },
          },
        },
        scales: {
          x: {
            display: true,
            grid: { display: false },
            ticks: {
              color: theme.tick,
              font: { family: theme.font, size: 10 },
            },
          },
          y: {
            display: true,
            beginAtZero: true,
            grid: { display: true, color: theme.grid },
            ticks: {
              color: theme.tick,
              font: { family: theme.font, size: 10 },
            },
          },
        },
      },
    });
    
    // SAVE INSTANCE
    canvas._chartInstance = newChart;
    component.charts.usageTrend = newChart;

  } catch (e) {
    console.error("Failed to create trend chart:", e);
  } finally {
    // Always release lock
    _trendChartUpdateLock = false;
  }
};
