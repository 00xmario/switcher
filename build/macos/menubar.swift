// Switcher menu bar app: supervises the bundled switcher server and shows
// per-account usage in the status bar menu. Single-file Swift, compiled
// with swiftc (no Xcode project).
import AppKit

let hubURL = URL(string: "http://127.0.0.1:8787")!
let launchAgentLabel = "sh.switcher.app"
let launchAgentPath = NSHomeDirectory() + "/Library/LaunchAgents/" + launchAgentLabel + ".plist"

struct UsageWindow: Codable {
    let label: String
    let used_percent: Int
    let resets_at: Double?
}

struct Usage: Codable {
    let available: Bool
    let windows: [UsageWindow]?
}

struct Account: Codable {
    let id: String
    let provider: String
    let email: String
    let plan: String?
    let active: Bool
    let exhausted_until: Double?
    let usage: Usage?
}

struct AppState: Codable {
    let active: [String: String]?
    let accounts: [Account]
    let order: [String]?
    let hidden: [String]?
}

let providerNames = ["codex": "Codex", "claude": "Claude", "grok": "Grok", "opencode": "OpenCode"]
let planNames = ["pro": "Pro 20x", "prolite": "Pro 5x", "plus": "Plus", "free": "Free"]

final class AppDelegate: NSObject, NSApplicationDelegate {
    let statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    let menu = NSMenu()
    var serverProcess: Process?

    func applicationDidFinishLaunching(_ notification: Notification) {
        ensureServerRunning()
        statusItem.button?.title = "⇄"
        statusItem.button?.toolTip = "Switcher"
        menu.autoenablesItems = false
        statusItem.menu = menu
        rebuildMenu()
        // The server refreshes usage itself; the menu just re-renders.
        Timer.scheduledTimer(withTimeInterval: 60, repeats: true) { [weak self] _ in
            self?.rebuildMenu()
        }
    }

    // MARK: - server supervision

    func serverReachable(timeout: Double) -> Bool {
        guard let data = fetchData(hubURL.appendingPathComponent("api/state"), timeout: timeout) else {
            return false
        }
        return !data.isEmpty
    }

    func ensureServerRunning() {
        if serverReachable(timeout: 1.5) { return }
        let server = URL(fileURLWithPath: Bundle.main.bundlePath + "/Contents/MacOS/SwitcherServer")
        guard FileManager.default.fileExists(atPath: server.path) else { return }
        let logDir = NSHomeDirectory() + "/.switcher"
        try? FileManager.default.createDirectory(atPath: logDir, withIntermediateDirectories: true)
        let process = Process()
        process.executableURL = server
        process.arguments = ["--port", "8787"]
        if let logHandle = FileHandle(forWritingAtPath: logDir + "/server.log") {
            process.standardOutput = logHandle
            process.standardError = logHandle
        }
        do {
            try process.run()
            serverProcess = process
            Thread.sleep(forTimeInterval: 0.8) // let it bind before the menu pulls
        } catch {
            NSLog("switcher: could not start server: \(error)")
        }
    }

    // MARK: - menu

    func rebuildMenu() {
        menu.removeAllItems()

        let header = NSMenuItem(title: "Switcher", action: nil, keyEquivalent: "")
        header.isEnabled = false
        menu.addItem(header)
        menu.addItem(.separator())

        let state = fetchState()
        if state == nil {
            let item = NSMenuItem(title: "Server unreachable", action: nil, keyEquivalent: "")
            item.isEnabled = false
            menu.addItem(item)
        } else {
            for providerID in visibleOrder(state) {
                let accounts = (state?.accounts ?? []).filter { $0.provider == providerID }
                let section = NSMenuItem(title: providerNames[providerID] ?? providerID, action: nil, keyEquivalent: "")
                section.isEnabled = false
                menu.addItem(section)
                if accounts.isEmpty {
                    let empty = NSMenuItem(title: "    No accounts", action: nil, keyEquivalent: "")
                    empty.isEnabled = false
                    menu.addItem(empty)
                    continue
                }
                for account in accounts {
                    for item in accountMenuItems(account) {
                        menu.addItem(item)
                    }
                }
            }
        }

        menu.addItem(.separator())
        let openItem = NSMenuItem(title: "Open web app", action: #selector(openWebApp), keyEquivalent: "")
        openItem.target = self
        menu.addItem(openItem)

        let startAtLogin = NSMenuItem(title: "Start at login", action: #selector(toggleStartAtLogin), keyEquivalent: "")
        startAtLogin.target = self
        startAtLogin.state = startAtLoginEnabled() ? .on : .off
        menu.addItem(startAtLogin)

        menu.addItem(.separator())
        let quitItem = NSMenuItem(title: "Quit Switcher", action: #selector(quit), keyEquivalent: "q")
        quitItem.target = self
        menu.addItem(quitItem)
    }

    func accountMenuItems(_ account: Account) -> [NSMenuItem] {
        var items: [NSMenuItem] = []
        let plan = account.plan.flatMap { planNames[$0] }.map { " · " + $0 } ?? ""
        var status = ""
        if let until = account.exhausted_until, Date(timeIntervalSince1970: until) > Date() {
            status = " · Out of usage"
        }
        let line = NSMenuItem(
            title: truncate(account.email, 34) + plan + status,
            action: nil, keyEquivalent: "")
        line.isEnabled = false
        if account.active { line.state = .on }
        items.append(line)
        for window in account.usage?.windows ?? [] {
            let left = max(0, min(100, 100 - window.used_percent))
            let usage = NSMenuItem(title: "    " + window.label + ": \(left)% left", action: nil, keyEquivalent: "")
            usage.isEnabled = false
            items.append(usage)
        }
        if !account.active {
            let use = NSMenuItem(title: "    Use this account", action: #selector(useAccount(_:)), keyEquivalent: "")
            use.target = self
            use.representedObject = account.id
            items.append(use)
        }
        return items
    }

    // MARK: - actions

    @objc func openWebApp() {
        NSWorkspace.shared.open(hubURL)
    }

    @objc func useAccount(_ sender: NSMenuItem) {
        guard let id = sender.representedObject as? String else { return }
        var request = URLRequest(url: hubURL.appendingPathComponent("api/accounts/\(id)/activate"))
        request.httpMethod = "POST"
        URLSession.shared.dataTask(with: request) { [weak self] _, _, _ in
            DispatchQueue.main.async { self?.rebuildMenu() }
        }.resume()
    }

    @objc func toggleStartAtLogin() {
        if startAtLoginEnabled() {
            try? FileManager.default.removeItem(atPath: launchAgentPath)
        } else {
            let exe = Bundle.main.bundlePath + "/Contents/MacOS/Switcher"
            let plist: [String: Any] = [
                "Label": launchAgentLabel,
                "ProgramArguments": [exe],
                "RunAtLoad": true,
            ]
            let dir = NSHomeDirectory() + "/Library/LaunchAgents"
            try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
            if let data = try? JSONSerialization.data(withJSONObject: plist, options: [.prettyPrinted]) {
                try? data.write(to: URL(fileURLWithPath: launchAgentPath))
            }
        }
        rebuildMenu()
    }

    @objc func quit() {
        if let process = serverProcess, process.isRunning {
            process.terminate()
        }
        NSApp.terminate(nil)
    }

    // MARK: - helpers

    let launchAgentPath = launchAgentLabel

    func fetchState(timeout: Double = 5) -> AppState? {
        guard let data = fetchData(hubURL.appendingPathComponent("api/state"), timeout: timeout) else {
            return nil
        }
        return try? JSONDecoder().decode(AppState.self, from: data)
    }

    func fetchData(_ url: URL, timeout: Double) -> Data? {
        var request = URLRequest(url: url)
        request.timeoutInterval = timeout
        let semaphore = DispatchSemaphore(value: 0)
        var payload: Data?
        URLSession.shared.dataTask(with: request) { data, _, _ in
            payload = data
            semaphore.signal()
        }.resume()
        _ = semaphore.wait(timeout: .now() + timeout)
        return payload
    }

    func visibleOrder(_ state: AppState?) -> [String] {
        let order = state?.order ?? ["codex", "claude", "grok", "opencode"]
        let hidden = Set(state?.hidden ?? [])
        return order.filter { !hidden.contains($0) }
    }

    func truncate(_ s: String, _ n: Int) -> String {
        s.count > n ? String(s.prefix(n)) + "…" : s
    }

    func startAtLoginEnabled() -> Bool {
        FileManager.default.fileExists(atPath: launchAgentPath)
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.accessory) // menu bar app: no Dock icon
app.run()
