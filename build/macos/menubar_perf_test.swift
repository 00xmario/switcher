import AppKit

@main
struct MenuPerformanceTest {
    static func main() {
        _ = NSApplication.shared
        let delegate = AppDelegate()
        let now = Date().timeIntervalSince1970
        let providers = ["claude", "claude", "codex", "codex", "opencode", "copilot"]
        let windows = [UsageWindow(label: "Session", used_percent: 70, resets_at: now + 14400),
            UsageWindow(label: "Weekly", used_percent: 35, resets_at: now + 350000),
            UsageWindow(label: "Monthly", used_percent: 30, resets_at: now + 700000)]
        delegate.cachedState = AppState(accounts: providers.enumerated().map { index, provider in
            Account(id: "fixture-\(index)", provider: provider, email: "account\(index)@example.test",
                plan: provider == "claude" ? "claude_max_5x" : "pro", active: index % 2 == 0,
                exhausted_until: nil, usage: Usage(available: true, windows: windows), reset_credits: nil,
                native_switch_available: provider == "claude", native_active: provider == "claude" && index == 1)
        }, order: ["claude", "codex", "opencode", "copilot"], hidden: [], version: "benchmark",
           update: nil, menu_usage_bars: true, reset_notifications: false)
        func measure(_ label: String, _ count: Int, _ action: () -> Void) {
            var times: [Double] = []
            for _ in 0..<count {
                let start = ProcessInfo.processInfo.systemUptime
                autoreleasepool { action() }
                times.append((ProcessInfo.processInfo.systemUptime - start) * 1000)
            }
            let warm = times.dropFirst().sorted()
            print(String(format: "%@: first=%.2f ms, warm p50=%.2f ms, p95=%.2f ms",
                label, times[0], warm[warm.count / 2], warm[Int(Double(warm.count - 1) * 0.95)]))
        }
        measure("Full rebuild, six fixture accounts", 31) { delegate.rebuildMenu() }
        measure("Menu-open preparation", 101) { delegate.prepareMenuForOpening() }
    }
}
