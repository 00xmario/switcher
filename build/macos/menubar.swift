// Switcher menu bar app: supervises the bundled switcher server and shows
// per-account usage in a custom-drawn dropdown (cards, toggles, logos).
// Single-file Swift, compiled with swiftc (no Xcode project).
import AppKit

let hubURL = URL(string: "http://127.0.0.1:8787")!
let launchAgentLabel = "sh.switcher.app"
let launchAgentPath = NSHomeDirectory() + "/Library/LaunchAgents/" + launchAgentLabel + ".plist"
let menuWidth: CGFloat = 320

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

struct UpdateInfo: Codable {
    let latest: String?
    let update_available: Bool?
}

struct AppState: Codable {
    let accounts: [Account]
    let order: [String]?
    let hidden: [String]?
    let version: String?
    let update: UpdateInfo?
}

let providerNames = ["codex": "Codex", "claude": "Claude", "grok": "Grok", "opencode": "OpenCode"]
let planNames = ["pro": "Pro 20x", "prolite": "Pro 5x", "plus": "Plus", "free": "Free"]

// palette
let inkColor = NSColor(red: 0.13, green: 0.12, blue: 0.11, alpha: 1)
let dimColor = NSColor(red: 0.48, green: 0.51, blue: 0.55, alpha: 1)
let accentColor = NSColor(red: 0.25, green: 0.61, blue: 0.44, alpha: 1)
let warnColor = NSColor(red: 0.85, green: 0.64, blue: 0.31, alpha: 1)
let surfaceColor = NSColor.controlBackgroundColor
let cardBorderColor = NSColor.systemGray.withAlphaComponent(0.35)

func label(_ text: String, font: NSFont, color: NSColor, alignment: NSTextAlignment = .left) -> NSTextField {
    let field = NSTextField(labelWithString: text)
    field.font = font
    field.textColor = color
    field.lineBreakMode = .byTruncatingTail
    return field
}

func cardView(width: CGFloat, height: CGFloat) -> NSView {
    let card = NSView(frame: NSRect(x: 0, y: 0, width: width, height: height))
    card.wantsLayer = true
    card.layer?.backgroundColor = surfaceColor.cgColor
    card.layer?.borderColor = cardBorderColor.cgColor
    card.layer?.borderWidth = 1
    card.layer?.cornerRadius = 10
    card.layer?.masksToBounds = true
    return card
}

func sectionLabel(_ text: String, width: CGFloat) -> NSView {
    let field = label(text.uppercased(), font: NSFont.systemFont(ofSize: 10.5, weight: .semibold), color: dimColor)
    field.frame = NSRect(x: 2, y: 0, width: width, height: 16)
    let row = NSView(frame: NSRect(x: 0, y: 0, width: width, height: 18))
    row.addSubview(field)
    return row
}

func textRow(width: CGFloat, text: String) -> NSView {
    let row = NSView(frame: NSRect(x: 0, y: 0, width: width, height: 36))
    let field = label(text, font: NSFont.systemFont(ofSize: 12.5), color: dimColor)
    field.frame = NSRect(x: 12, y: 10, width: width - 24, height: 16)
    row.addSubview(field)
    return row
}

func roundButton(width: CGFloat, title: String, target: AnyObject?, action: Selector) -> NSView {
    let card = cardView(width: width, height: 36)
    let button = NSButton(title: title, target: target, action: action)
    button.isBordered = false
    button.font = NSFont.systemFont(ofSize: 13, weight: .medium)
    button.frame = NSRect(x: 0, y: 9, width: width, height: 20)
    button.alignment = .center
    card.addSubview(button)
    return card
}

func menuItemWithView(_ view: NSView) -> NSMenuItem {
    let item = NSMenuItem()
    item.view = view
    item.isEnabled = false
    return item
}

func visibleOrder(_ state: AppState?) -> [String] {
    let order = state?.order ?? ["codex", "claude", "grok", "opencode"]
    let hidden = Set(state?.hidden ?? [])
    return order.filter { !hidden.contains($0) }
}

func truncate(_ s: String, _ n: Int) -> String {
    s.count > n ? String(s.prefix(n)) + "…" : s
}

// Hover-highlighting, clickable row. Tapping invokes the handler.
final class ClickableRow: NSView {
    var onClicked: (() -> Void)?

    override func mouseDown(with event: NSEvent) {
        if let handler = onClicked { handler() }
    }

    override func mouseEntered(with event: NSEvent) {
        wantsLayer = true
        layer?.backgroundColor = NSColor.systemGray.withAlphaComponent(0.12).cgColor
    }

    override func mouseExited(with event: NSEvent) {
        layer?.backgroundColor = NSColor.clear.cgColor
    }
}

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
        Timer.scheduledTimer(withTimeInterval: 60, repeats: true) { [weak self] _ in
            self?.rebuildMenu()
        }
    }

    // MARK: server supervision

    func serverReachable(timeout: Double) -> Bool {
        fetchState(timeout: timeout) != nil
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
            Thread.sleep(forTimeInterval: 0.8)
        } catch {
            NSLog("switcher: could not start server: \(error)")
        }
    }

    // MARK: data

    func fetchState(timeout: Double = 5) -> AppState? {
        var request = URLRequest(url: hubURL.appendingPathComponent("api/state"))
        request.timeoutInterval = timeout
        let semaphore = DispatchSemaphore(value: 0)
        var decoded: AppState?
        URLSession.shared.dataTask(with: request) { data, _, _ in
            if let data = data { decoded = try? JSONDecoder().decode(AppState.self, from: data) }
            semaphore.signal()
        }.resume()
        _ = semaphore.wait(timeout: .now() + timeout)
        return decoded
    }

    func post(path: String, completion: (() -> Void)? = nil) {
        var request = URLRequest(url: hubURL.appendingPathComponent(path))
        request.httpMethod = "POST"
        URLSession.shared.dataTask(with: request) { _, _, _ in completion?() }.resume()
    }

    // MARK: menu construction

    func rebuildMenu() {
        menu.removeAllItems()

        // Header: centered logo mark, like the inspiration shot.
        let headerView = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 60))
        let logo = NSImageView(frame: NSRect(x: (menuWidth - 42) / 2, y: 8, width: 42, height: 42))
        logo.image = Bundle.main.image(forResource: "AppIcon")
        headerView.addSubview(logo)
        menu.addItem(menuItemWithView(headerView))

        let state = fetchState()
        if state == nil {
            menu.addItem(menuItemWithView(textRow(width: menuWidth, text: "Server unreachable")))
        } else {
            for providerID in visibleOrder(state) {
                let accounts = (state?.accounts ?? []).filter { $0.provider == providerID }
                if accounts.isEmpty { continue } // dead sections stay out of the menu
                menu.addItem(menuItemWithView(sectionLabel(providerNames[providerID] ?? providerID, width: menuWidth)))
                menu.addItem(menuItemWithView(providerCard(providerID: providerID, accounts: accounts)))
            }
        }

        menu.addItem(.separator())

        // Bottom action buttons.
        let installAvailable = state?.update?.update_available == true
        let updateTitle = installAvailable ? "Install update" : "Check for updates"
        let actionRow = NSStackView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 42))
        actionRow.orientation = .horizontal
        actionRow.distribution = .fillEqually
        actionRow.spacing = 8
        actionRow.edgeInsets = NSEdgeInsets(top: 4, left: 10, bottom: 8, right: 10)
        let webButton = roundButton(width: (menuWidth - 38) / 2, title: "Open web app", target: self, action: #selector(openWebApp))
        let updateButton = roundButton(width: (menuWidth - 38) / 2, title: updateTitle, target: self, action: #selector(runUpdate))
        actionRow.addArrangedSubview(webButton)
        actionRow.addArrangedSubview(updateButton)
        menu.addItem(menuItemWithView(actionRow))

        // Start at login with a real toggle switch.
        let toggleCard = cardView(width: menuWidth, height: 42)
        let field = label("Start at login", font: NSFont.systemFont(ofSize: 13), color: inkColor)
        field.frame = NSRect(x: 12, y: 13, width: menuWidth - 80, height: 16)
        toggleCard.addSubview(field)
        let toggle = NSSwitch(frame: NSRect(x: menuWidth - 58, y: 8, width: 44, height: 26))
        toggle.state = startAtLoginEnabled() ? .on : .off
        toggle.target = self
        toggle.action = #selector(toggleStartAtLogin)
        toggleCard.addSubview(toggle)
        menu.addItem(menuItemWithView(toggleCard))

        let quitItem = NSMenuItem(title: "Quit Switcher", action: #selector(quit), keyEquivalent: "q")
        quitItem.target = self
        menu.addItem(quitItem)
    }

    func providerCard(providerID: String, accounts: [Account]) -> NSView {
        let height = accounts.reduce(CGFloat(8)) { $0 + rowHeight(for: $1) }
        let card = cardView(width: menuWidth, height: height)
        var y = height
        for (index, account) in accounts.enumerated() {
            y -= rowHeight(for: account)

            let row = ClickableRow(frame: NSRect(x: 0, y: y, width: menuWidth, height: rowHeight(for: account)))
            row.onClicked = { [weak self] in
                self?.post(path: "api/accounts/\(account.id)/activate") { [weak self] in
                    DispatchQueue.main.async { self?.rebuildMenu() }
                }
            }

            let logo = NSImageView(frame: NSRect(x: 12, y: rowHeight(for: account) / 2 - 12, width: 24, height: 24))
            let logoImage = NSImage(named: NSImage.Name("logo-" + providerID))
            logoImage?.size = NSSize(width: 24, height: 24)
            logo.image = logoImage
            if providerID == "grok" || providerID == "opencode" { logoImage?.isTemplate = true }
            row.addSubview(logo)

            let plan = account.plan.flatMap { planNames[$0] }.map { " · " + $0 } ?? ""
            let title = label(truncate(account.email, 24) + plan,
                font: NSFont.systemFont(ofSize: 12.5, weight: account.active ? .medium : .regular),
                color: account.active ? accentColor : inkColor)
            title.frame = NSRect(x: 44, y: rowHeight(for: account) - 18, width: menuWidth - 110, height: 15)
            row.addSubview(title)

            var usageY = rowHeight(for: account) - 32
            for window in account.usage?.windows ?? [] {
                let left = max(0, min(100, 100 - window.used_percent))
                let usage = label("   " + window.label + ": \(left)% left",
                    font: NSFont.systemFont(ofSize: 11), color: dimColor)
                usage.frame = NSRect(x: 44, y: usageY, width: menuWidth - 110, height: 14)
                row.addSubview(usage)
                usageY -= 15
            }

            if account.active {
                let check = NSImageView(frame: NSRect(x: menuWidth - 26, y: rowHeight(for: account) / 2 - 6, width: 13, height: 13))
                check.image = NSImage(systemSymbolName: "checkmark", accessibilityDescription: nil)
                check.contentTintColor = accentColor
                row.addSubview(check)
            } else if let until = account.exhausted_until, Date(timeIntervalSince1970: until) > Date() {
                let badge = label("Out of usage", font: NSFont.systemFont(ofSize: 10.5), color: warnColor)
                badge.alignment = .right
                badge.frame = NSRect(x: menuWidth - 96, y: rowHeight(for: account) - 18, width: 86, height: 14)
                row.addSubview(badge)
            }

            if index < accounts.count - 1 {
                let divider = NSView(frame: NSRect(x: 12, y: y, width: menuWidth - 24, height: 1))
                divider.wantsLayer = true
                divider.layer?.backgroundColor = cardBorderColor.cgColor
                card.addSubview(divider)
            }
            card.addSubview(row)
        }
        return card
    }

    func rowHeight(for account: Account) -> CGFloat {
        let windows = CGFloat(account.usage?.windows?.count ?? 0)
        return 24 + windows * 15
    }

    // MARK: actions

    @objc func openWebApp() {
        NSWorkspace.shared.open(hubURL)
    }

    @objc func runUpdate() {
        post(path: "api/update") { [weak self] in
            DispatchQueue.main.async {
                // The server exec-restarts; give it a beat, then refresh.
                Thread.sleep(forTimeInterval: 1.5)
                self?.rebuildMenu()
            }
        }
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
        if let process = serverProcess, process.isRunning { process.terminate() }
        NSApp.terminate(nil)
    }

    func startAtLoginEnabled() -> Bool {
        FileManager.default.fileExists(atPath: launchAgentPath)
    }

}

// menuItemWithView attaches a custom view to a menu item.






let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.accessory) // menu bar app: no Dock icon
app.run()
