/**
 * Classifier Audit Feed Component
 * Real-time SSE stream of safety monitor interception events.
 */
window.Components = window.Components || {};

window.Components.classifierAuditFeed = () => ({
    auditEvents: [],
    eventSource: null,
    AUDIT_LIMIT: 50,
    init() {
        if (this.$watch) {
            this.$watch('$store.global.settingsTab', (tab) => {
                if (tab === 'classifier') {
                    this.connectAuditStream();
                } else {
                    this.disconnectAuditStream();
                }
            });
        }
        if (Alpine.store('global')?.settingsTab === 'classifier') {
            this.connectAuditStream();
        }
    },
    connectAuditStream() {
        this.disconnectAuditStream();
        const password = Alpine.store('global')?.webuiPassword;
        const url = password
            ? `/api/classifier/audit/stream?history=true&password=${encodeURIComponent(password)}`
            : '/api/classifier/audit/stream?history=true';

        this.eventSource = new EventSource(url);
        this.eventSource.onmessage = (event) => {
            try {
                const parsed = JSON.parse(event.data);
                this.auditEvents.unshift(parsed);
                if (this.auditEvents.length > this.AUDIT_LIMIT) {
                    this.auditEvents.length = this.AUDIT_LIMIT;
                }
            } catch (err) {
                console.error('Failed to parse classifier audit event:', err);
            }
        };
        this.eventSource.onerror = () => {
            // EventSource reconnects on its own
        };
    },
    disconnectAuditStream() {
        if (this.eventSource) {
            this.eventSource.close();
            this.eventSource = null;
        }
    },
    statusClass(status) {
        switch (status) {
            case 'rerouted': return 'text-green-400 border-green-400/40 bg-green-400/10';
            case 'stubbed': return 'text-yellow-400 border-yellow-400/40 bg-yellow-400/10';
            case 'error': return 'text-red-400 border-red-400/40 bg-red-400/10';
            default: return 'text-gray-400 border-gray-400/40 bg-gray-400/10';
        }
    }
});

document.addEventListener('alpine:init', () => {
    Alpine.data('classifierAuditFeed', window.Components.classifierAuditFeed);
});
