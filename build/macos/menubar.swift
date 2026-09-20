// Switcher menu bar app: supervises the bundled switcher server and shows
// per-account usage in a custom-drawn dropdown (cards, toggles, logos).
// Single-file Swift, compiled with swiftc (no Xcode project).
import AppKit

let hubURL = URL(string: "http://127.0.0.1:8787")!
let launchAgentLabel = "sh.switcher.app"
let launchAgentPath = NSHomeDirectory() + "/Library/LaunchAgents/" + launchAgentLabel + ".plist"
let menuWidth: CGFloat = 340
let edgeInset: CGFloat = 16      // menu-level padding (left + right)
let cardInset: CGFloat = 14      // padding inside cards

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

let inkColor = NSColor(red: 0.13, green: 0.12, blue: 0.11, alpha: 1)
let dimColor = NSColor(red: 0.48, green: 0.51, blue: 0.55, alpha: 1)
let accentColor = NSColor(red: 0.25, green: 0.61, blue: 0.44, alpha: 1)
let warnColor = NSColor(red: 0.85, green: 0.64, blue: 0.31, alpha: 1)
let surfaceColor = NSColor.controlBackgroundColor
let cardBorderColor = NSColor.systemGray.withAlphaComponent(0.35)

func label(_ text: String, font: NSFont, color: NSColor) -> NSTextField {
    let field = NSTextField(labelWithString: text)
    field.font = font
    field.textColor = color
    field.lineBreakMode = .byTruncatingTail
    field.alignment = .left
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

func sectionHead(_ providerID: String, contentWidth: CGFloat) -> NSView {
    let head = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 26))
    let logo = NSImageView(frame: NSRect(x: edgeInset, y: 4, width: 18, height: 18))
    let image = providerLogoImage(providerID)
    image?.size = NSSize(width: 18, height: 18)
    logo.image = image
    let name = label(providerNames[providerID] ?? providerID,
        font: NSFont.systemFont(ofSize: 13, weight: .semibold), color: inkColor)
    name.frame = NSRect(x: edgeInset + 26, y: 5, width: contentWidth - 18 - 10, height: 16)
    head.addSubview(logo)
    head.addSubview(name)
    return head
}

// providerLogoImage loads the bundled mark, monochrome ones as templates
// so they follow dark mode.
func providerLogoImage(_ providerID: String) -> NSImage? {
    guard let url = Bundle.main.url(forResource: providerID, withExtension: "png") else {
        return nil
    }
    let image = NSImage(contentsOf: url)
    if providerID == "grok" || providerID == "opencode" { image?.isTemplate = true }
    return image
}

// Hover-highlighting, clickable row. Tapping invokes the handler.
final class ClickableRow: NSView {
    var onClicked: (() -> Void)?
    private var tracking: NSTrackingArea?

    override func layout() {
        super.layout()
        if tracking == nil {
            wantsLayer = true
            layer?.cornerRadius = 8
            layer?.masksToBounds = true
            addTrackingArea(NSTrackingArea(rect: bounds,
                options: [.mouseEnteredAndExited, .activeInKeyWindow, .inVisibleRect], owner: self))
            tracking = NSTrackingArea()
        }
    }

    override func mouseDown(with event: NSEvent) {
        if let handler = onClicked { handler() }
    }

    override func mouseEntered(with event: NSEvent) {
        layer?.backgroundColor = NSColor.systemGray.withAlphaComponent(0.10).cgColor
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

    func ensureServerRunning() {
        if fetchState(timeout: 1.5) != nil { return }
        let server = URL(fileURLWithPath: Bundle.main.bundlePath + "/Contents/MacOS/SwitcherServer")
        guard FileManager.default.fileExists(atPath: server.path) else { return }
        let logDir = NSHomeDirectory() + "/.switcher"
        try? FileManager.default.createDirectory(atPath: logDir, withIntermediateDirectories: true)
        if !FileManager.default.fileExists(atPath: logDir + "/server.log") {
            FileManager.default.createFile(atPath: logDir + "/server.log", contents: nil)
        }
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
        let contentWidth = menuWidth - 2 * edgeInset

        // Header: centered logo + version.
        let state = fetchState()
        let headerView = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 66))
        let logo = NSImageView(frame: NSRect(x: (menuWidth - 42) / 2, y: 10, width: 42, height: 42))
        logo.image = Bundle.main.image(forResource: "AppIcon")
        headerView.addSubview(logo)
        let versionField = label("v" + (state?.version ?? "0.0.0"),
            font: NSFont.systemFont(ofSize: 10.5), color: dimColor)
        versionField.alignment = .center
        versionField.frame = NSRect(x: 0, y: 0, width: menuWidth, height: 12)
        headerView.addSubview(versionField)
        menu.addItem(menuItemWithView(headerView))

        if state == nil {
            let row = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 36))
            let field = label("Server unreachable", font: NSFont.systemFont(ofSize: 12.5), color: dimColor)
            field.frame = NSRect(x: edgeInset, y: 10, width: contentWidth, height: 16)
            row.addSubview(field)
            menu.addItem(menuItemWithView(row))
        } else {
            let visible = (state?.order ?? ["codex", "claude", "grok", "opencode"])
                .filter { !(state?.hidden ?? []).contains($0) }
                .filter { providerID in
                    !(state?.accounts ?? []).filter { $0.provider == providerID }.isEmpty
                }
            for (position, providerID) in visible.enumerated() {
                if position > 0 {
                    menu.addItem(menuItemWithView(spacerView(height: 12)))
                }
                let accounts = (state?.accounts ?? []).filter { $0.provider == providerID }
                menu.addItem(menuItemWithView(sectionHead(providerID, contentWidth: contentWidth)))
                menu.addItem(menuItemWithView(providerCard(providerID: providerID, accounts: accounts, contentWidth: contentWidth)))
            }
        }

        menu.addItem(.separator())

        // Bottom actions: equal-width buttons, text vertically centered.
        let installAvailable = state?.update?.update_available == true
        let updateTitle = installAvailable ? "Install update" : "Check for updates"
        let buttonWidth = (menuWidth - 2 * edgeInset - 10) / 2
        let actionRow = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 46))
        let webCard = cardView(width: buttonWidth, height: 38)
        webCard.frame.origin = NSPoint(x: edgeInset, y: 4)
        let webButton = NSButton(title: "Open web app", target: self, action: #selector(openWebApp))
        webButton.isBordered = false
        webButton.font = NSFont.systemFont(ofSize: 13, weight: .medium)
        webButton.frame = NSRect(x: 0, y: 9, width: buttonWidth, height: 20)
        webButton.alignment = .center
        webCard.addSubview(webButton)
        actionRow.addSubview(webCard)
        let updateCard = cardView(width: buttonWidth, height: 38)
        updateCard.frame.origin = NSPoint(x: edgeInset + buttonWidth + 10, y: 4)
        let updateButton = NSButton(title: updateTitle,
            target: self, action: #selector(runUpdate))
        updateButton.isBordered = false
        updateButton.font = NSFont.systemFont(ofSize: 13, weight: .medium)
        updateButton.frame = NSRect(x: 0, y: 9, width: buttonWidth, height: 20)
        updateButton.alignment = .center
        updateCard.addSubview(updateButton)
        actionRow.addSubview(updateCard)
        menu.addItem(menuItemWithView(actionRow))

        // Start at login: symmetric card, label left, toggle right.
        let toggleCard = cardView(width: contentWidth, height: 42)
        let field = label("Start at login", font: NSFont.systemFont(ofSize: 13), color: inkColor)
        field.frame = NSRect(x: cardInset, y: 13, width: 180, height: 16)
        toggleCard.addSubview(field)
        let toggle = NSSwitch(frame: NSRect(x: contentWidth - cardInset - 48, y: 9, width: 44, height: 26))
        toggle.state = startAtLoginEnabled() ? .on : .off
        toggle.target = self
        toggle.action = #selector(toggleStartAtLogin)
        toggleCard.addSubview(toggle)
        menu.addItem(menuItemWithView(toggleCard))

        let quitItem = NSMenuItem(title: "Quit Switcher", action: #selector(quit), keyEquivalent: "q")
        quitItem.target = self
        menu.addItem(quitItem)
    }

    func providerCard(providerID: String, accounts: [Account], contentWidth: CGFloat) -> NSView {
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

            let logo = NSImageView(frame: NSRect(x: cardInset, y: rowHeight(for: account) / 2 - 11, width: 22, height: 22))
            let logoImage = providerLogoImage(providerID)
            logoImage?.size = NSSize(width: 22, height: 22)
            logo.image = logoImage
            row.addSubview(logo)

            // Email + plan, LEFT aligned at the same leading inset as the
            // logo column: text at logo right + 10.
            let textX = cardInset + 22 + 10
            let plan = account.plan.flatMap { planNames[$0] }.map { " · " + $0 } ?? ""
            let title = label(account.email + plan,
                font: NSFont.systemFont(ofSize: 12.5, weight: account.active ? .medium : .regular),
                color: account.active ? accentColor : inkColor)
            let textWidth = contentWidth - textX - cardInset - 24
            title.frame = NSRect(x: textX, y: rowHeight(for: account) - 19, width: textWidth, height: 15)
            row.addSubview(title)

            var usageY = rowHeight(for: account) - 33
            for window in account.usage?.windows ?? [] {
                let left = max(0, min(100, 100 - window.used_percent))
                let usage = label(window.label + ": \(left)% left",
                    font: NSFont.systemFont(ofSize: 11), color: dimColor)
                usage.frame = NSRect(x: textX, y: usageY, width: textWidth, height: 14)
                row.addSubview(usage)
                usageY -= 15
            }

            if account.active {
                let check = NSImageView(frame: NSRect(x: contentWidth - cardInset - 14, y: rowHeight(for: account) / 2 - 6, width: 14, height: 14))
                check.image = NSImage(systemSymbolName: "checkmark", accessibilityDescription: nil)
                check.contentTintColor = accentColor
                row.addSubview(check)
            } else if let until = account.exhausted_until, Date(timeIntervalSince1970: until) > Date() {
                let badge = label("Out of usage", font: NSFont.systemFont(ofSize: 10.5), color: warnColor)
                badge.alignment = .right
                badge.frame = NSRect(x: contentWidth - cardInset - 90, y: rowHeight(for: account) - 19, width: 90, height: 14)
                row.addSubview(badge)
            }

            if index < accounts.count - 1 {
                let divider = NSView(frame: NSRect(x: cardInset, y: y, width: contentWidth - 2 * cardInset, height: 1))
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


func spacerView(height: CGFloat) -> NSView {
    NSView(frame: NSRect(x: 0, y: 0, width: 1, height: height))
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.accessory) // menu bar app: no Dock icon
app.run()
