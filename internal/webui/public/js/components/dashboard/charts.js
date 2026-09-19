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

// Color palette for different families and models
const FAMILY_COLORS = {
  get claude() {
    return getThemeColor("--color-neon-purple");
  },
  get gemini() {
    return getThemeColor("--color-neon-green");
  },
  get other() {
    return getThemeColor("--color-neon-cyan");
  },
};

const MODEL_COLORS = Array.from({ length: 16 }, (_, i) =>
  getThemeColor(`--color-chart-${i + 1}`)
);

// Blueprint discipline: one trace hue on the trend chart; families are
// split by dash pattern (solid = claude, dashed = gemini, dotted = other),
// never by color.
const TRACE_COLOR = () => getThemeColor("--color-neon-green") || "#5ec8d8";
const FAMILY_DASH = {
  claude: [],
  gemini: [6, 4],
  other: [2, 3],
};

// Export constants for filter module
window.DashboardConstants = { FAMILY_COLORS, MODEL_COLORS, FAMILY_DASH, TRACE_COLOR };

// Module-level lock to prevent concurrent chart updates (fixes race condition)
let _trendChartUpdateLock = false;

/**
 * Convert hex color to rgba
 * @param {string} hex - Hex color string
 * @param {number} alpha - Alpha value (0-1)
 * @returns {string} rgba color string
 */
window.DashboardCharts.hexToRgba = function (hex, alpha) {
  const result = /^#?([a-f\d]{2})([a-f\d]{2})([a-f\d]{2})$/i.exec(hex);
  if (result) {
    return `rgba(${parseInt(result[1], 16)}, ${parseInt(
      result[2],
      16
    )}, ${parseInt(result[3], 16)}, ${alpha})`;
  }
  return hex;
};

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
 * Create a Chart.js dataset with gradient fill
 * @param {string} label - Dataset label
 * @param {Array} data - Data points
 * @param {string} color - Line color
 * @param {HTMLCanvasElement} canvas - Canvas element
 * @param {Array} [dash] - borderDash pattern (empty = solid)
 * @returns {object} Chart.js dataset configuration
 */
window.DashboardCharts.createDataset = function (label, data, color, canvas, dash) {
  let gradient;

  try {
    // Safely create gradient with fallback
    if (canvas && canvas.getContext) {
      const ctx = canvas.getContext("2d");
      if (ctx && ctx.createLinearGradient) {
        gradient = ctx.createLinearGradient(0, 0, 0, 200);
        gradient.addColorStop(0, window.DashboardCharts.hexToRgba(color, 0.12));
        gradient.addColorStop(
          0.6,
          window.DashboardCharts.hexToRgba(color, 0.05)
        );
        gradient.addColorStop(1, "rgba(0, 0, 0, 0)");
      }
    }
  } catch (e) {
    if (window.UILogger) window.UILogger.debug("Gradient fallback:", e.message);
    gradient = null;
  }

  // Fallback to solid color if gradient creation failed
  const backgroundColor =
    gradient || window.DashboardCharts.hexToRgba(color, 0.08);

  return {
    label,
    data,
    borderColor: color,
    backgroundColor: backgroundColor,
    borderWidth: 1.5,
    borderDash: dash || [],
    tension: 0.35,
    fill: true,
    pointRadius: 2.5,
    pointHoverRadius: 6,
    pointBackgroundColor: color,
    pointBorderColor: "rgba(14, 27, 42, 0.9)",
    pointBorderWidth: 1.5,
  };
};

/**
 * Update quota distribution donut chart
 * @param {object} component - Dashboard component instance
 */
window.DashboardCharts.updateCharts = function (component) {
  const canvas = document.getElementById("quotaChart");

  // Safety checks
  if (!canvas) {
    console.debug("quotaChart canvas not found");
    return;
  }

  // FORCE DESTROY: Check for existing chart on the canvas element property
  // This handles cases where Component state is lost but DOM persists
  if (canvas._chartInstance) {
    console.debug("Destroying existing quota chart from canvas property");
    try {
        canvas._chartInstance.destroy();
    } catch(e) { if (window.UILogger) window.UILogger.debug(e); }
    canvas._chartInstance = null;
  }
  
  // Also check component state as backup
  if (component.charts.quotaDistribution) {
     try {
         component.charts.quotaDistribution.destroy();
     } catch(e) { }
     component.charts.quotaDistribution = null;
  }
  
  // Also try Chart.js registry
  if (typeof Chart !== "undefined" && Chart.getChart) {
      const regChart = Chart.getChart(canvas);
      if (regChart) {
          try { regChart.destroy(); } catch(e) {}
      }
  }

  if (typeof Chart === "undefined") {
    if (window.UILogger) window.UILogger.warn("Chart.js not loaded");
    return;
  }
  if (!isCanvasReady(canvas)) {
    if (window.UILogger) window.UILogger.debug("quotaChart canvas not ready, skipping update");
    return;
  }

  // Use UNFILTERED data for global health chart
  const rows = Alpine.store("data").getUnfilteredQuotaData();
  if (!rows || rows.length === 0) return;

  const healthByFamily = {};
  let totalHealthSum = 0;
  let totalModelCount = 0;

  rows.forEach((row) => {
    const family = row.family || "unknown";
    if (!healthByFamily[family]) {
      healthByFamily[family] = { total: 0, weighted: 0 };
    }

    // Calculate average health from quotaInfo (each entry has { pct }).
    // Null pct = unknown quota: excluded from the average so unknowns
    // never drag health down with a false 0%.
    const quotaInfo = row.quotaInfo || [];
    let avgHealth = 0;

    const known = quotaInfo.filter((q) => q && q.pct !== null && q.pct !== undefined);
    if (known.length > 0) {
      avgHealth = known.reduce((sum, q) => sum + q.pct, 0) / known.length;
    } else if (quotaInfo.length > 0) {
      return; // all-unknown row: exclude from health aggregates entirely
    }
    // If quotaInfo is empty, avgHealth remains 0 (depleted/unknown)

    healthByFamily[family].total++;
    healthByFamily[family].weighted += avgHealth;
    totalHealthSum += avgHealth;
    totalModelCount++;
  });

  // Update overall health for dashboard display
  component.stats.overallHealth = totalModelCount > 0
    ? Math.round(totalHealthSum / totalModelCount)
    : 0;

  // Quota gauge segments: active quota uses the trace hue per family
  // (claude/gemini distinguishable), depleted quota fades the same hue out.
  const familyColors = {
    claude: TRACE_COLOR(),
    gemini: getThemeColor("--color-neon-purple") || "#8fb8dc",
    unknown: getThemeColor("--color-text-tertiary") || "#9fb4c9",
  };

  const data = [];
  const colors = [];
  const labels = [];

  const totalFamilies = Object.keys(healthByFamily).length;
  const segmentSize = 100 / totalFamilies;

  Object.entries(healthByFamily).forEach(([family, { total, weighted }]) => {
    const health = weighted / total;
    const activeVal = (health / 100) * segmentSize;
    const inactiveVal = segmentSize - activeVal;

    const familyColor = familyColors[family] || familyColors["unknown"];

    // Get translation keys
    const store = Alpine.store("global");
    const familyKey =
      "family" + family.charAt(0).toUpperCase() + family.slice(1);
    const familyName = store.t(familyKey);

    // Labels using translations if possible
    const activeLabel =
      family === "claude"
        ? store.t("claudeActive")
        : family === "gemini"
        ? store.t("geminiActive")
        : `${familyName} ${store.t("activeSuffix")}`;

    const depletedLabel =
      family === "claude"
        ? store.t("claudeEmpty")
        : family === "gemini"
        ? store.t("geminiEmpty")
        : `${familyName} ${store.t("depleted")}`;

    // Active segment
    data.push(activeVal);
    colors.push(familyColor);
    labels.push(activeLabel);

    // Inactive segment
    data.push(inactiveVal);
    // Depleted quota fades the family hue almost into the sheet ground.
    colors.push(window.DashboardCharts.hexToRgba(familyColor, 0.25));
    labels.push(depletedLabel);
  });

  // Create Chart
  try {
    const newChart = new Chart(canvas, {
       // ... config
       type: "doughnut",
       data: {
         labels: labels,
         datasets: [
           {
             data: data,
             backgroundColor: colors,
             borderColor: getThemeColor("--color-space-950"),
             borderWidth: 0,
             hoverOffset: 0,
             borderRadius: 0,
           },
         ],
       },
       options: {
         responsive: true,
         maintainAspectRatio: false,
         cutout: "85%",
         rotation: -90,
         circumference: 360,
         plugins: {
           legend: { display: false },
           tooltip: { enabled: false },
           title: { display: false },
         },
         animation: {
           // Disable animation for quota chart to prevent "double refresh" visual glitch
           duration: 0
         },
       },
    });
    
    // SAVE INSTANCE TO CANVAS AND COMPONENT
    canvas._chartInstance = newChart;
    component.charts.quotaDistribution = newChart;
    
  } catch (e) {
    console.error("Failed to create quota chart:", e);
  }
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
          canvas,
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
            canvas
          )
        );
      });
    });
  }

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
            backgroundColor:
              getThemeColor("--color-space-950") || "rgba(24, 24, 27, 0.9)",
            titleColor: getThemeColor("--color-text-main"),
            bodyColor: getThemeColor("--color-text-bright"),
            borderColor: getThemeColor("--color-space-border"),
            borderWidth: 1,
            padding: 10,
            displayColors: true,
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
              color: getThemeColor("--color-text-muted"),
              font: { size: 10 },
            },
          },
          y: {
            display: true,
            beginAtZero: true,
            grid: {
              display: true,
              color:
                getThemeColor("--color-space-border") + "1a" ||
                "rgba(255,255,255,0.05)",
            },
            ticks: {
              color: getThemeColor("--color-text-muted"),
              font: { size: 10 },
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
