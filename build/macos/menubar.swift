// Switcher menu bar app: supervises the bundled switcher server and shows
// per-account usage in a custom-drawn dropdown (cards, toggles, logos).
// Stack-view based so alignment is guaranteed. Single-file Swift, compiled
// with swiftc (no Xcode project).
import AppKit

let hubURL = URL(string: "http://127.0.0.1:8787")!
let launchAgentLabel = "sh.switcher.app"
let launchAgentPath = NSHomeDirectory() + "/Library/LaunchAgents/" + launchAgentLabel + ".plist"
let menuWidth: CGFloat = 340
let edgeInset: CGFloat = 16

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
    field.alignment = alignment
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

// Hover-highlighting, clickable row. Tapping invokes the handler.
final class ClickableRow: NSView {
    var onClicked: (() -> Void)?
    private var tracking: NSTrackingArea?

    override func layout() {
        super.layout()
        if tracking == nil {
            let area = NSTrackingArea(rect: bounds,
                options: [.mouseEnteredAndExited, .activeInKeyWindow, .inVisibleRect], owner: self)
            addTrackingArea(area)
            tracking = area
            wantsLayer = true
            layer?.cornerRadius = 8
            layer?.masksToBounds = true
        }
    }

    override func mouseDown(with event: NSEvent) {
        if let handler = onClicked { handler() }
    }

    override func mouseEntered(with event: NSEvent) {
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

    func ensureServerRunning() {
        if fetchState(timeout: 1.5) != nil { return }
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
        let headerView = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 74))
        let logo = NSImageView(frame: NSRect(x: (menuWidth - 44) / 2, y: 12, width: 44, height: 44))
        logo.image = Bundle.main.image(forResource: "AppIcon")
        headerView.addSubview(logo)
        let versionField = label(
            "v" + (fetchState(timeout: 3)?.version ?? "0.0.0"),
            font: NSFont.systemFont(ofSize: 10.5), color: dimColor)
        versionField.alignment = .center
        versionField.frame = NSRect(x: 0, y: 0, width: menuWidth, height: 12)
        headerView.addSubview(versionField)
        menu.addItem(menuItemWithView(headerView))

        let state = fetchState()
        if state == nil {
            let row = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 36))
            let field = label("Server unreachable", font: NSFont.systemFont(ofSize: 12.5), color: dimColor)
            field.frame = NSRect(x: edgeInset, y: 10, width: menuWidth - 2 * edgeInset, height: 16)
            row.addSubview(field)
            menu.addItem(menuItemWithView(row))
        } else {
            let visible = visibleOrder(state).filter { providerID in
                !(state?.accounts ?? []).filter { $0.provider == providerID }.isEmpty
            }
            for (position, providerID) in visible.enumerated() {
                if position > 0 {
                    // breathing room between providers
                    let spacer = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 12))
                    menu.addItem(menuItemWithView(spacer))
                }
                let accounts = (state?.accounts ?? []).filter { $0.provider == providerID }
                let head = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 24))
                let logoStack = NSStackView(frame: NSRect(x: 0, y: 0, width: menuWidth - 2 * edgeInset, height: 24))
                logoStack.orientation = .horizontal
                logoStack.spacing = 8
                logoStack.alignment = .centerY
                logoStack.edgeInsets = NSEdgeInsets(top: 0, left: 0, bottom: 0, right: 0)
                let logo = NSImageView(frame: NSRect(x: 0, y: 0, width: 18, height: 18))
                let image = NSImage(named: NSImage.Name("logo-" + providerID))
                image?.size = NSSize(width: 18, height: 18)
                if providerID == "grok" || providerID == "opencode" { image?.isTemplate = true }
                logo.image = image
                let nameField = label(providerNames[providerID] ?? providerID,
                    font: NSFont.systemFont(ofSize: 13, weight: .semibold), color: inkColor)
                logoStack.addArrangedSubview(logo)
                logoStack.addArrangedSubview(nameField)
                head.addSubview(logoStack)
                menu.addItem(menuItemWithView(head))
                menu.addItem(menuItemWithView(providerCard(providerID: providerID, accounts: accounts)))
            }
        }

        menu.addItem(.separator())

        // Bottom action buttons.
        let installAvailable = state?.update?.update_available == true
        let updateTitle = installAvailable ? "Install update" : "Check for updates"
        let actionRow = NSStackView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 46))
        actionRow.orientation = .horizontal
        actionRow.distribution = .fillEqually
        actionRow.spacing = 10
        actionRow.edgeInsets = NSEdgeInsets(top: 6, left: edgeInset, bottom: 10, right: edgeInset)
        actionRow.addArrangedSubview(roundButton(width: (menuWidth - 2 * edgeInset - 10) / 2,
            title: "Open web app", target: self, action: #selector(openWebApp)))
        actionRow.addArrangedSubview(roundButton(width: (menuWidth - 2 * edgeInset - 10) / 2,
            title: updateTitle, target: self, action: #selector(runUpdate)))
        menu.addItem(menuItemWithView(actionRow))

        // Start at login with a real toggle.
        let toggleCard = cardView(width: menuWidth - 2 * edgeInset, height: 44)
        let field = label("Start at login", font: NSFont.systemFont(ofSize: 13), color: inkColor)
        field.frame = NSRect(x: 12, y: 14, width: 180, height: 16)
        toggleCard.addSubview(field)
        let toggle = NSSwitch(frame: NSRect(x: menuWidth - 2 * edgeInset - 58, y: 9, width: 44, height: 26))
        toggle.state = startAtLoginEnabled() ? .on : .off
        toggle.target = self
        toggle.action = #selector(toggleStartAtLogin)
        toggleCard.addSubview(toggle)
        let toggleItem = NSMenuItem()
        toggleItem.view = toggleCard
        menu.addItem(toggleItem)

        let quitItem = NSMenuItem(title: "Quit Switcher", action: #selector(quit), keyEquivalent: "q")
        quitItem.target = self
        menu.addItem(quitItem)
    }

    func providerCard(providerID: String, accounts: [Account]) -> NSView {
        let contentWidth = menuWidth - 2 * edgeInset
        let height = accounts.reduce(CGFloat(4)) { $0 + rowHeight(for: $1) }
        let card = cardView(width: contentWidth, height: height)
        var y = height
        for (index, account) in accounts.enumerated() {
            y -= rowHeight(for: account)

            let row = ClickableRow(frame: NSRect(x: 0, y: y, width: contentWidth, height: rowHeight(for: account)))
            row.onClicked = { [weak self] in
                self?.post(path: "api/accounts/\(account.id)/activate") { [weak self] in
                    DispatchQueue.main.async { self?.rebuildMenu() }
                }
            }

            let logo = NSImageView(frame: NSRect(x: 12, y: rowHeight(for: account) / 2 - 11, width: 22, height: 22))
            let logoImage = NSImage(named: NSImage.Name("logo-" + providerID))
            logoImage?.size = NSSize(width: 22, height: 22)
            logo.image = logoImage
            if providerID == "grok" || providerID == "opencode" { logoImage?.isTemplate = true }
            row.addSubview(logo)

            let plan = account.plan.flatMap { planNames[$0] }.map { " · " + $0 } ?? ""
            let title = label(truncate(account.email, 26) + plan,
                font: NSFont.systemFont(ofSize: 12.5, weight: account.active ? .medium : .regular),
                color: account.active ? accentColor : inkColor)
            title.frame = NSRect(x: 46, y: rowHeight(for: account) - 19, width: contentWidth - 130, height: 15)
            row.addSubview(title)

            var usageY = rowHeight(for: account) - 33
            for window in account.usage?.windows ?? [] {
                let left = max(0, min(100, 100 - window.used_percent))
                let usage = label(window.label + ": \(left)% left",
                    font: NSFont.systemFont(ofSize: 11), color: dimColor)
                usage.frame = NSRect(x: 46, y: usageY, width: contentWidth - 130, height: 14)
                row.addSubview(usage)
                usageY -= 15
            }

            if account.active {
                let check = NSImageView(frame: NSRect(x: contentWidth - 26, y: rowHeight(for: account) / 2 - 6, width: 13, height: 13))
                check.image = NSImage(systemSymbolName: "checkmark", accessibilityDescription: nil)
                check.contentTintColor = accentColor
                row.addSubview(check)
            } else if let until = account.exhausted_until, Date(timeIntervalSince1970: until) > Date() {
                let badge = label("Out of usage", font: NSFont.systemFont(ofSize: 10.5), color: warnColor)
                badge.alignment = .right
                badge.frame = NSRect(x: contentWidth - 100, y: rowHeight(for: account) - 19, width: 88, height: 14)
                row.addSubview(badge)
            }

            if index < accounts.count - 1 {
                let divider = NSView(frame: NSRect(x: 12, y: y, width: contentWidth - 24, height: 1))
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
        return 30 + windows * 15
    }

    // MARK: actions

    @objc func openWebApp() {
        NSWorkspace.shared.open(hubURL)
    }

    @objc func runUpdate() {
        post(path: "api/update") { [weak self] in
            DispatchQueue.main.async {
                Thread.sleep(forTimeInterval: 1.5) // the server exec-restarts
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

// MARK: - top-level view builders

func menuItemWithView(_ view: NSView) -> NSMenuItem {
    let item = NSMenuItem()
    item.view = view
    item.isEnabled = false
    return item
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.accessory) // menu bar app: no Dock icon
app.run()

func visibleOrder(_ state: AppState?) -> [String] {
    let order = state?.order ?? ["codex", "claude", "grok", "opencode"]
    let hidden = Set(state?.hidden ?? [])
    return order.filter { !hidden.contains($0) }
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

func truncate(_ s: String, _ n: Int) -> String {
    s.count > n ? String(s.prefix(n)) + "…" : s
}
