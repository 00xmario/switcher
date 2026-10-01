// Switcher menu bar app: supervises the bundled switcher server and shows
// per-account usage in a custom-drawn dropdown (cards, toggles, logos).
// Single-file Swift, compiled with swiftc (no Xcode project).
import AppKit
import UserNotifications

let hubURL = URL(string: "http://127.0.0.1:8787")!

// The server can rotate this device token while the app is running.
func currentDeviceToken() -> String? {
    guard let raw = try? String(contentsOfFile: NSHomeDirectory() + "/.switcher/local-token", encoding: .utf8) else { return nil }
    let token = raw.trimmingCharacters(in: .whitespacesAndNewlines)
    return token.count == 64 ? token : nil
}

// The menu app and server share this local preference file. Consult it at
// delivery time as well as the cached state so a web toggle takes effect
// without waiting for the menu app's next state poll.
func resetAlertsEnabledOnDisk() -> Bool {
    let path = NSHomeDirectory() + "/.switcher/settings.json"
    guard let data = try? Data(contentsOf: URL(fileURLWithPath: path)),
          let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { return false }
    return object["reset_notifications"] as? Bool == true
}
let launchAgentLabel = "sh.switcher.app"
let launchAgentPath = NSHomeDirectory() + "/Library/LaunchAgents/" + launchAgentLabel + ".plist"
let menuWidth: CGFloat = 340
let edgeInset: CGFloat = 16      // menu-level padding (left + right)
let cardInset: CGFloat = 14      // padding inside cards

struct UsageWindow: Codable, Equatable {
    let label: String
    let used_percent: Int
    let resets_at: Double?
}

struct Usage: Codable, Equatable {
    let available: Bool
    let windows: [UsageWindow]?
}

struct ResetCredits: Codable, Equatable {
    let count: Int
}

func bankedResetText(_ credits: ResetCredits?) -> String? {
    guard let count = credits?.count, count > 0 else { return nil }
    return "⚡ \(count) banked"
}

struct Account: Codable, Equatable {
    let id: String
    let provider: String
    let email: String
    let plan: String?
    let active: Bool
    let exhausted_until: Double?
    let usage: Usage?
    let reset_credits: ResetCredits?
    var native_switch_available: Bool? = nil
    var native_active: Bool? = nil
    var selected: Bool { native_switch_available == true ? native_active == true : active }
}

struct UpdateInfo: Codable, Equatable {
    let latest: String?
    let update_available: Bool?
}

struct AppState: Codable, Equatable {
    let accounts: [Account]
    let order: [String]?
    let hidden: [String]?
    let version: String?
    let update: UpdateInfo?
    let menu_usage_bars: Bool?
    let reset_notifications: Bool?
}

private let resetPrefix = "sh.switcher.reset."
private let resetCatchupInterval: TimeInterval = 24 * 3600
private let resetRolloverLead: TimeInterval = 5 * 60

struct ResetAlert: Codable, Equatable {
    let id: String
    let accountID: String
    let providerID: String
    let provider: String
    let window: String
    let date: Date
}

struct ResetAlertLedger: Codable, Equatable {
    let alerts: [ResetAlert]
    let deliveredIDs: [String]
}

struct ResetAlertPermission: Equatable {
    let message: String
    let canSend: Bool
}

func resetAlertPermission(_ status: UNAuthorizationStatus, alertSetting: UNNotificationSetting,
                          alertStyle: UNAlertStyle, centerSetting: UNNotificationSetting) -> ResetAlertPermission {
    switch status {
    case .authorized where alertSetting == .enabled && alertStyle != .none:
        return ResetAlertPermission(message: "macOS allows banners (Focus may silence)", canSend: true)
    case .authorized where centerSetting == .enabled:
        return ResetAlertPermission(message: "Reset alerts go to Notification Center", canSend: true)
    case .authorized:
        return ResetAlertPermission(message: "Reset alerts hidden in macOS Settings", canSend: false)
    case .provisional where centerSetting == .enabled:
        return ResetAlertPermission(message: "Reset alerts delivered quietly by macOS", canSend: true)
    case .provisional:
        return ResetAlertPermission(message: "Reset alerts hidden in macOS Settings", canSend: false)
    case .denied:
        return ResetAlertPermission(message: "Reset alerts blocked in macOS Settings", canSend: false)
    case .notDetermined:
        return ResetAlertPermission(message: "Waiting for notification permission", canSend: false)
    @unknown default:
        return ResetAlertPermission(message: "Check macOS notification permission", canSend: false)
    }
}

func resetAlertPermission(_ settings: UNNotificationSettings) -> ResetAlertPermission {
    resetAlertPermission(settings.authorizationStatus, alertSetting: settings.alertSetting,
        alertStyle: settings.alertStyle, centerSetting: settings.notificationCenterSetting)
}

func sendTestResetAlert(completion: @escaping (String?) -> Void) {
    guard resetAlertsEnabledOnDisk() else {
        completion("Enable reset alerts in Switcher Settings first")
        return
    }
    let center = UNUserNotificationCenter.current()
    center.getNotificationSettings { settings in
        let permission = resetAlertPermission(settings)
        guard permission.canSend, resetAlertsEnabledOnDisk() else {
            completion(permission.canSend ? "Reset alerts were turned off" : permission.message)
            return
        }
        let content = UNMutableNotificationContent()
        content.title = "Switcher reset alert test"
        content.body = "Switcher can send macOS notifications."
        content.sound = .default
        center.add(UNNotificationRequest(identifier: "sh.switcher.test." + UUID().uuidString,
            content: content, trigger: nil)) { error in completion(error?.localizedDescription) }
    }
}

func loadResetAlertLedger(at url: URL) throws -> ResetAlertLedger {
    guard FileManager.default.fileExists(atPath: url.path) else {
        return ResetAlertLedger(alerts: [], deliveredIDs: [])
    }
    let decoder = JSONDecoder()
    decoder.dateDecodingStrategy = .secondsSince1970
    return try decoder.decode(ResetAlertLedger.self, from: Data(contentsOf: url))
}

func saveResetAlertLedger(_ ledger: ResetAlertLedger, at url: URL) throws {
    let encoder = JSONEncoder()
    encoder.dateEncodingStrategy = .secondsSince1970
    try encoder.encode(ledger).write(to: url, options: .atomic)
    try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
}

func desiredResetAlerts(_ state: AppState, now: Date = Date()) -> [ResetAlert] {
    guard state.reset_notifications == true else { return [] }
    let end = now.addingTimeInterval(30 * 86400)
    var alerts: [ResetAlert] = []
    for account in state.accounts where account.usage?.available == true {
        for (index, window) in (account.usage?.windows ?? []).enumerated() {
            guard let seconds = window.resets_at else { continue }
            let date = Date(timeIntervalSince1970: seconds)
            guard date > now && date <= end else { continue }
            alerts.append(ResetAlert(id: resetPrefix + account.id + ".\(index).\(Int(seconds))",
                accountID: account.id, providerID: account.provider,
                provider: providerNames[account.provider] ?? account.provider, window: window.label, date: date))
        }
    }
    return Array(alerts.sorted { $0.date == $1.date ? $0.id < $1.id : $0.date < $1.date }.prefix(32))
}

// A provider can roll a used window to the next period before the menu's
// timer runs. Keep an alert near its reported time while the same account
// and window still exist, even if usage is already back to zero.
func reconciledResetAlerts(_ previous: [ResetAlert], state: AppState, now: Date) -> [ResetAlert] {
    guard state.reset_notifications == true else { return [] }
    let upcoming = desiredResetAlerts(state, now: now)
    let carried = previous.filter { alert in
        let delay = now.timeIntervalSince(alert.date)
        guard delay >= -resetRolloverLead && delay < resetCatchupInterval,
              let account = state.accounts.first(where: { $0.id == alert.accountID && $0.provider == alert.providerID }) else {
            return false
        }
        // An unavailable usage response cannot revoke an already planned
        // reset. A successful response that removed the window can.
        guard account.usage?.available == true else { return true }
        return (account.usage?.windows ?? []).contains { $0.label == alert.window }
    }
    let carriedIDs = Set(carried.map(\.id))
    return Array((carried + upcoming.filter { !carriedIDs.contains($0.id) }).prefix(32))
}

func dueResetAlerts(_ alerts: [ResetAlert], now: Date, delivered: Set<String>, sending: Set<String>) -> [ResetAlert] {
    alerts.filter { alert in
        let delay = now.timeIntervalSince(alert.date)
        return delay >= 0 && delay < resetCatchupInterval && !delivered.contains(alert.id) && !sending.contains(alert.id)
    }
}

func recordDeliveredResetAlert(_ id: String, alerts: inout [ResetAlert], delivered: inout Set<String>) {
    delivered.insert(id)
    alerts.removeAll { $0.id == id }
}

let providerNames = ["codex": "Codex", "claude": "Claude", "grok": "Grok", "opencode": "OpenCode",
    "antigravity": "Antigravity", "gemini": "Gemini", "copilot": "Copilot"]
let planNames = ["claude_max_5x": "Max 5x", "claude_max_20x": "Max 20x", "claude_max": "Max", "claude_pro": "Pro",
    "max": "Max", "pro": "Pro 20x", "prolite": "Pro 5x", "plus": "Plus", "free": "Free",
    "copilot_pro": "Pro", "copilot_pro_plus": "Pro+", "copilot_business": "Business", "copilot_enterprise": "Enterprise",
    "copilot_free": "Free"]

// Palette mirrors the web app's light and dark tokens (web/style.css).
// Layer colours cannot be dynamic NSColors, so the menu picks a palette from
// the effective appearance every time it is rebuilt.
struct Palette {
    let ink: NSColor
    let dim: NSColor
    let accent: NSColor
    let surface: NSColor
    let border: NSColor
    let hover: NSColor
    let dark: Bool

    static let light = Palette(
        ink: NSColor(hex: 0x17191d), dim: NSColor(hex: 0x7a818c), accent: NSColor(hex: 0x2f9e5f),
        surface: NSColor(hex: 0xffffff), border: NSColor(hex: 0xe4e4e8),
        hover: NSColor.black.withAlphaComponent(0.05), dark: false)
    static let dark = Palette(
        ink: NSColor(hex: 0xf5f5f5), dim: NSColor(hex: 0xa3a3a3), accent: NSColor(hex: 0x3fb96f),
        surface: NSColor(hex: 0x151515), border: NSColor(hex: 0x242424),
        hover: NSColor.white.withAlphaComponent(0.06), dark: true)

    static func current() -> Palette {
        let appearance = NSApp?.effectiveAppearance ?? NSAppearance.currentDrawing()
        return appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua ? dark : light
    }
}

extension NSColor {
    convenience init(hex: Int) {
        self.init(red: CGFloat((hex >> 16) & 0xff) / 255, green: CGFloat((hex >> 8) & 0xff) / 255,
            blue: CGFloat(hex & 0xff) / 255, alpha: 1)
    }
}

var palette = Palette.current()
var inkColor: NSColor { palette.ink }
var dimColor: NSColor { palette.dim }
var accentColor: NSColor { palette.accent }
var surfaceColor: NSColor { palette.surface }
var cardBorderColor: NSColor { palette.border }
let warnColor = NSColor(red: 0.85, green: 0.64, blue: 0.31, alpha: 1)

func label(_ text: String, font: NSFont, color: NSColor) -> NSTextField {
    let field = NSTextField(labelWithString: text)
    field.font = font
    field.textColor = color
    field.lineBreakMode = .byTruncatingTail
    field.alignment = .left
    return field
}

let centeredParagraph: NSParagraphStyle = {
    let style = NSMutableParagraphStyle()
    style.alignment = .center
    return style
}()

func cardView(width: CGFloat, height: CGFloat) -> NSView {
    let card = HoverCard(frame: NSRect(x: 0, y: 0, width: width, height: height))
    card.wantsLayer = true
    card.layer?.backgroundColor = surfaceColor.cgColor
    card.layer?.borderColor = cardBorderColor.cgColor
    card.layer?.borderWidth = 1
    card.layer?.cornerRadius = 10
    card.layer?.masksToBounds = true
    return card
}

func sectionHead(_ providerID: String, contentWidth: CGFloat, syncPhase: String = "idle", onSync: (() -> Void)? = nil) -> NSView {
    let head = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 32))
    let logo = NSImageView(frame: NSRect(x: edgeInset, y: 5, width: 22, height: 22))
    logo.imageScaling = .scaleProportionallyUpOrDown
    logo.image = providerLogoImage(providerID)
    logo.contentTintColor = inkColor
    let name = label(providerNames[providerID] ?? providerID,
        font: NSFont.systemFont(ofSize: 13, weight: .semibold), color: inkColor)
    let nameWidth = providerID == "claude"
        ? ceil(textWidth(name.stringValue, font: name.font!)) : contentWidth - 32
    name.frame = NSRect(x: edgeInset + 22 + 10, y: 8, width: nameWidth, height: 16)
    head.addSubview(logo)
    head.addSubview(name)
    if providerID == "claude" {
        let button = ClaudeSyncButton(phase: syncPhase, onSync: onSync)
        button.frame = NSRect(x: name.frame.maxX + 6, y: 3, width: 26, height: 26)
        head.addSubview(button)
    }
    return head
}

// A native secondary button with a small progress indicator. Unlike the
// usage refresh animation, its busy state lasts for the actual request.
final class ClaudeSyncButton: NSButton {
    private let onSync: (() -> Void)?
    private var tracking: NSTrackingArea?
    init(phase: String, onSync: (() -> Void)?) {
        self.onSync = onSync
        super.init(frame: NSRect(x: 0, y: 0, width: 26, height: 26))
        isBordered = false
        title = ""
        setAccessibilityLabel("Sync Claude Code sessions between accounts")
        toolTip = "Sync Claude Code sessions between accounts\nConfirmation required: this closes and reopens Claude Desktop."
        target = self
        action = #selector(clicked)
        wantsLayer = true
        layer?.cornerRadius = 7
        let symbol = phase == "success" ? "checkmark" : phase == "error" ? "exclamationmark.circle" : "arrow.left.arrow.right"
        image = NSImage(systemSymbolName: symbol, accessibilityDescription: nil)
        symbolConfiguration = NSImage.SymbolConfiguration(pointSize: 13, weight: .medium)
        contentTintColor = phase == "success" ? accentColor : phase == "error" ? NSColor.systemRed : dimColor
        isEnabled = phase != "running"
        if phase == "running" {
            image = nil
            let spinner = NSProgressIndicator(frame: NSRect(x: 6, y: 6, width: 14, height: 14))
            spinner.style = .spinning
            spinner.controlSize = .small
            spinner.startAnimation(nil)
            addSubview(spinner)
            setAccessibilityValue("Syncing")
        }
    }
    required init?(coder: NSCoder) { fatalError("unused") }
    override func updateTrackingAreas() {
        super.updateTrackingAreas()
        if let tracking = tracking { removeTrackingArea(tracking) }
        tracking = NSTrackingArea(rect: bounds, options: [.mouseEnteredAndExited, .activeAlways, .inVisibleRect], owner: self)
        if let tracking = tracking { addTrackingArea(tracking) }
    }
    override func mouseEntered(with event: NSEvent) { if isEnabled { layer?.backgroundColor = palette.hover.cgColor } }
    override func mouseExited(with event: NSEvent) { layer?.backgroundColor = NSColor.clear.cgColor }
    @objc private func clicked() { onSync?() }
}

struct ClaudeSyncResponse: Decodable {
    let detail: String?
    let error: String?
    let details: String?
    let result: SyncResult?
    struct SyncResult: Decodable { let backup: String? }
}

struct AccountActivationResponse: Decodable {
    let error: String?
    let details: String?
    let backup: String?
    let native: NativeResult?
    struct NativeResult: Decodable { let message: String? }
}

func claudeSyncConfirmation() -> NSAlert {
    let alert = NSAlert()
    alert.messageText = "Sync Claude Code sessions?"
    alert.informativeText = "This will close and reopen Claude Desktop, even if it is running in the background. Finish any active work first. Switcher will back up the Desktop session indexes and copy only missing session pointers between accounts. Existing records and conversation transcripts stay unchanged."
    alert.addButton(withTitle: "Cancel")
    alert.addButton(withTitle: "Sync sessions")
    alert.buttons[0].keyEquivalent = "\r"
    alert.buttons[1].keyEquivalent = ""
    return alert
}

// Copilot's single-path mark and Grok's glyph follow the current palette;
// other provider marks retain their own colours.
func logoUsesTemplate(_ providerID: String) -> Bool {
    providerID == "grok" || providerID == "copilot"
}

// providerLogoImage loads the same provider marks used by the web app.
func providerLogoImage(_ providerID: String) -> NSImage? {
    let key = "\(providerID)|\(palette.dark)" as NSString
    if let cached = menuLogoCache.object(forKey: key) { return cached }
    guard let url = Bundle.main.url(forResource: providerID, withExtension: "svg") else {
        return nil
    }
    guard let image = NSImage(contentsOf: url) else { return nil }
    image.isTemplate = logoUsesTemplate(providerID)
    let result = providerID == "opencode" && palette.dark
        ? invertedImage(image, size: NSSize(width: 22, height: 22)) ?? image : image
    menuLogoCache.setObject(result, forKey: key)
    return result
}

private let menuImageContext = CIContext()
private let menuLogoCache: NSCache<NSString, NSImage> = {
    let cache = NSCache<NSString, NSImage>(); cache.countLimit = 32; return cache
}()
private let menuBlurCache: NSCache<NSString, NSImage> = {
    let cache = NSCache<NSString, NSImage>(); cache.countLimit = 128; cache.totalCostLimit = 8 * 1024 * 1024; return cache
}()
private let menuTextWidths: NSCache<NSString, NSNumber> = {
    let cache = NSCache<NSString, NSNumber>(); cache.countLimit = 512; return cache
}()
private let menuResetFormatter = DateFormatter()
private var menuResetFormatKey = ""

// invertedImage renders an image and inverts its colours (dark mode
// counterpart of the web app's CSS invert on the OpenCode mark).
func invertedImage(_ image: NSImage, size: NSSize) -> NSImage? {
    let view = NSImageView(frame: NSRect(origin: .zero, size: size))
    view.image = image
    view.imageScaling = .scaleProportionallyUpOrDown
    guard let rep = renderBitmap(view), let cg = rep.cgImage,
          let filter = CIFilter(name: "CIColorInvert") else { return nil }
    let input = CIImage(cgImage: cg)
    filter.setValue(input, forKey: kCIInputImageKey)
    // Invert colour only; keep the original alpha so transparent areas stay transparent.
    guard let inverted = filter.outputImage,
          let masked = CIFilter(name: "CIBlendWithAlphaMask", parameters: [
              kCIInputImageKey: inverted, kCIInputBackgroundImageKey: CIImage.empty(), kCIInputMaskImageKey: input,
          ])?.outputImage,
          let result = menuImageContext.createCGImage(masked, from: input.extent) else { return nil }
    return NSImage(cgImage: result, size: size)
}

// resetRemaining renders the countdown until a usage window resets, the
// same formatting the web app uses: 38m, 6h 12m, or 2d 3h. Nil when there
// is no reset time or it has already passed.
func resetRemaining(_ untilUnix: Double?) -> String? {
    guard let until = untilUnix, until > 0 else { return nil }
    let ms = until * 1000 - Date().timeIntervalSince1970 * 1000
    if ms <= 0 { return nil }
    let m = Int(ms / 60000)
    if m < 60 { return "\(m)m" }
    let h = m / 60
    if h < 24 { return "\(h)h \(m % 60)m" }
    let d = h / 24
    return "\(d)d \(h % 24)h"
}

func exactResetTime(_ untilUnix: Double?) -> String? {
    guard let untilUnix = untilUnix, untilUnix.isFinite,
          untilUnix > Date().timeIntervalSince1970 else { return nil }
    let formatter = menuResetFormatter
    let formatKey = Locale.current.identifier + "|" + TimeZone.current.identifier
    if menuResetFormatKey != formatKey {
        formatter.locale = .current
        formatter.dateStyle = .full
        formatter.timeStyle = .full
        formatter.timeZone = .current
        menuResetFormatKey = formatKey
    }
    let date = Date(timeIntervalSince1970: untilUnix)
    let offset = TimeZone.current.secondsFromGMT(for: date)
    let sign = offset >= 0 ? "+" : "-"
    let hours = abs(offset) / 3600
    let minutes = abs(offset) % 3600 / 60
    return "Provider-reported reset: " + formatter.string(from: date) +
        String(format: " (UTC%@%02d:%02d)", sign, hours, minutes)
}

// textWidth measures the frame width a label needs for a string, using a
// real NSTextField so the field's own padding is included.
func textWidth(_ text: String, font: NSFont) -> CGFloat {
    let key = String(reflecting: [text, font.fontName, String(describing: font.pointSize),
        String(describing: font.fontDescriptor.object(forKey: .traits))]) as NSString
    if let cached = menuTextWidths.object(forKey: key) { return CGFloat(cached.doubleValue) }
    let probe = NSTextField(labelWithString: text)
    probe.font = font
    probe.sizeToFit()
    let width = ceil(probe.frame.width) + 2
    menuTextWidths.setObject(NSNumber(value: Double(width)), forKey: key)
    return width
}

struct UsageLineLayout {
    let label: NSRect
    let value: NSRect
    let reset: NSRect?
    let resetText: String?
    let bar: NSRect?
}

struct UsageLayout {
    let lines: [UsageLineLayout]
    let checkmark: NSRect
}

// Layout is shared by rendering and a geometry check. All frames use the
// account row's coordinates so title actions and usage lines can be checked
// for collisions before views are created.
func usageLayout(windows: [UsageWindow], titleY: CGFloat,
                 lineWidth: CGFloat, contentWidth: CGFloat, textX: CGFloat,
                 cardInset: CGFloat, lineHeight: CGFloat, titleGap: CGFloat,
                 showBars: Bool) -> UsageLayout {
    let usageFont = NSFont.systemFont(ofSize: 11)
    let valueFont = NSFont.monospacedDigitSystemFont(ofSize: 11, weight: .regular)
    let resetFont = NSFont.systemFont(ofSize: 10.5)
    let resetTexts = windows.map { resetRemaining($0.resets_at).map { "↻ " + $0 } }
    let resetColumn = resetTexts.compactMap { $0 }
        .map { textWidth($0, font: resetFont) }.max() ?? 0
    let resetSpace = resetColumn > 0 ? resetColumn + 12 : 0
    let minValueWidth = textWidth("100% left", font: valueFont)
    let maxLabelWidth = max(0, lineWidth - 5 - resetSpace - minValueWidth)
    let labelColumn = min(windows.map { textWidth($0.label + ":", font: usageFont) }.max() ?? 0,
                          maxLabelWidth)
    // A tiny indicator fits before the percentage without increasing row
    // height. If a provider supplies a longer window label, keep the text
    // legible and omit the bar for that account instead of squeezing it.
    let barWidth: CGFloat = 38
    let barGap: CGFloat = 8
    let valueSpace = lineWidth - labelColumn - 5 - resetSpace
    let barsFit = showBars && valueSpace >= barWidth + barGap + minValueWidth
    var usageY = titleY - titleGap - lineHeight
    let lines = resetTexts.map { reset -> UsageLineLayout in
        let valueX = textX + labelColumn + 5 + (barsFit ? barWidth + barGap : 0)
        let line = UsageLineLayout(
            label: NSRect(x: textX, y: usageY, width: labelColumn, height: lineHeight),
            value: NSRect(x: valueX, y: usageY,
                          width: valueSpace - (barsFit ? barWidth + barGap : 0),
                          height: lineHeight),
            reset: reset.map { _ in NSRect(x: contentWidth - cardInset - resetColumn,
                                          y: usageY + 0.5, width: resetColumn, height: lineHeight) },
            resetText: reset,
            bar: barsFit ? NSRect(x: textX + labelColumn + 5, y: usageY + (lineHeight - 3) / 2,
                                  width: barWidth, height: 3) : nil)
        usageY -= lineHeight
        return line
    }
    // Keep the active mark on the account title line, above every usage
    // row, rather than centering it over a countdown.
    let checkmark = NSRect(x: contentWidth - cardInset - 14, y: titleY + 0.5,
                           width: 14, height: 14)
    return UsageLayout(lines: lines, checkmark: checkmark)
}

// ClickableRow is one account line. Hover state is driven by the card
// that contains it (see HoverCard), so rows can never get out of sync.
final class ClickableRow: NSView {
    var onClicked: (() -> Void)?
    var onHover: ((Bool) -> Void)?
    private(set) var hovered = false

    override init(frame: NSRect) {
        super.init(frame: frame)
        wantsLayer = true
        layer?.cornerRadius = 8
        layer?.masksToBounds = true
    }

    required init?(coder: NSCoder) { fatalError("unused") }

    func setHovered(_ value: Bool) {
        guard value != hovered else { return }
        hovered = value
        layer?.backgroundColor = value ? palette.hover.cgColor : NSColor.clear.cgColor
        onHover?(value)
    }

    override func mouseDown(with event: NSEvent) {
        if let handler = onClicked { handler() }
    }
}

// HoverCard holds account rows. Hover is not driven by AppKit tracking
// areas (menus drop and reorder enter/exit events); the delegate polls the
// cursor while the menu is open and calls updateHover.
final class HoverCard: NSView {
    private func rows() -> [ClickableRow] { subviews.compactMap { $0 as? ClickableRow } }

    func updateHover(screenPoint: NSPoint) {
        guard let window = window else { return }
        let point = convert(window.convertPoint(fromScreen: screenPoint), from: nil)
        for row in rows() { row.setHovered(row.frame.contains(point)) }
    }

    func clearHover() { rows().forEach { $0.setHovered(false) } }
}

// renderBitmap draws a view into a Retina bitmap so it can be turned into a
// static image (used for the blurred email and the spinner icon).
func renderBitmap(_ view: NSView) -> NSBitmapImageRep? {
    let size = view.bounds.size
    guard let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: Int(size.width * 2), pixelsHigh: Int(size.height * 2),
        bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
        colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0) else { return nil }
    rep.size = size
    view.cacheDisplay(in: view.bounds, to: rep)
    return rep
}

// blurredImage renders a label once through CIGaussianBlur. Same privacy
// pattern as the web app: unreadable until hovered.
let blurRadius: Double = 5

func blurredImage(of field: NSTextField) -> NSImage? {
    let key = String(reflecting: [field.stringValue, String(describing: field.bounds.size),
        field.font?.fontName ?? "", String(describing: field.font?.pointSize),
        String(describing: field.font?.fontDescriptor.object(forKey: .traits)),
        String(describing: field.textColor?.usingColorSpace(.deviceRGB)), String(palette.dark), "scale=2"]) as NSString
    if let cached = menuBlurCache.object(forKey: key) { return cached }
    guard let rep = renderBitmap(field), let cg = rep.cgImage else { return nil }
    let input = CIImage(cgImage: cg)
    guard let clamp = CIFilter(name: "CIAffineClamp"), let blur = CIFilter(name: "CIGaussianBlur") else { return nil }
    clamp.setValue(input, forKey: kCIInputImageKey)
    clamp.setValue(CGAffineTransform.identity, forKey: kCIInputTransformKey)
    blur.setValue(clamp.outputImage, forKey: kCIInputImageKey)
    blur.setValue(blurRadius * 2, forKey: kCIInputRadiusKey) // radius in 2x pixels
    guard let output = blur.outputImage?.cropped(to: input.extent),
          let result = menuImageContext.createCGImage(output, from: input.extent) else { return nil }
    let image = NSImage(cgImage: result, size: field.bounds.size)
    menuBlurCache.setObject(image, forKey: key, cost: result.bytesPerRow * result.height)
    return image
}

// BlurredLabel stacks a blurred snapshot above the real label and
// cross-fades layer opacity on hover. Layer animations are used on purpose:
// AppKit's animator() stalls inside the menu tracking run loop.
final class BlurredLabel: NSView {
    private let veil = NSImageView()
    private let field: NSTextField

    init(field: NSTextField) {
        self.field = field
        super.init(frame: field.frame)
        wantsLayer = true
        field.frame.origin = .zero
        field.wantsLayer = true
        addSubview(field)
        veil.frame = bounds
        veil.imageScaling = .scaleNone
        veil.image = blurredImage(of: field)
        veil.wantsLayer = true
        addSubview(veil)
        field.layer?.opacity = 0
    }

    required init?(coder: NSCoder) { fatalError("unused") }

    func reveal(_ show: Bool) {
        fade(veil.layer, to: show ? 0 : 1)
        fade(field.layer, to: show ? 1 : 0)
    }

    private func fade(_ layer: CALayer?, to target: Float) {
        guard let layer = layer else { return }
        let animation = CABasicAnimation(keyPath: "opacity")
        animation.fromValue = layer.presentation()?.opacity ?? layer.opacity
        animation.toValue = target
        animation.duration = 0.18
        animation.timingFunction = CAMediaTimingFunction(name: .easeOut)
        layer.removeAnimation(forKey: "fade")
        layer.opacity = target
        layer.add(animation, forKey: "fade")
    }
}

// ToggleSwitch is a custom on/off control drawn in the app accent. NSSwitch
// cannot be tinted and renders grey inside menus (inactive window style).
final class ToggleSwitch: NSView {
    var isOn: Bool { didSet { render(animated: true) } }
    var onChanged: ((Bool) -> Void)?
    private let knob = CALayer()

    init(isOn: Bool) {
        self.isOn = isOn
        super.init(frame: NSRect(x: 0, y: 0, width: 40, height: 24))
        wantsLayer = true
        layer?.cornerRadius = 12
        knob.frame = CGRect(x: 2, y: 2, width: 20, height: 20)
        knob.cornerRadius = 10
        knob.backgroundColor = NSColor.white.cgColor
        knob.shadowColor = NSColor.black.cgColor
        knob.shadowOpacity = 0.18
        knob.shadowRadius = 1.5
        knob.shadowOffset = CGSize(width: 0, height: -0.5)
        layer?.addSublayer(knob)
        render(animated: false)
    }

    required init?(coder: NSCoder) { fatalError("unused") }

    private func render(animated: Bool) {
        CATransaction.begin()
        CATransaction.setDisableActions(!animated)
        CATransaction.setAnimationDuration(0.18)
        layer?.backgroundColor = (isOn ? accentColor : NSColor.systemGray.withAlphaComponent(0.35)).cgColor
        knob.frame.origin.x = isOn ? bounds.width - 22 : 2
        CATransaction.commit()
    }

    override func mouseDown(with event: NSEvent) {
        isOn.toggle()
        onChanged?(isOn)
    }
}

// SpinnerButton shows an SF Symbol in a plain CALayer (so AppKit never
// resets its anchor point) and spins it around its center while an action
// runs.
final class SpinnerButton: NSView {
    var onClicked: (() -> Void)?
    private let iconLayer = CALayer()

    init(symbol: String, size: CGFloat, color: NSColor) {
        super.init(frame: NSRect(x: 0, y: 0, width: size, height: size))
        wantsLayer = true
        let config = NSImage.SymbolConfiguration(pointSize: size * 0.58, weight: .medium)
            .applying(NSImage.SymbolConfiguration(paletteColors: [color]))
        let symbolView = NSImageView(frame: bounds)
        symbolView.image = NSImage(systemSymbolName: symbol, accessibilityDescription: nil)?
            .withSymbolConfiguration(config)
        if let rep = renderBitmap(symbolView) {
            iconLayer.contents = rep.cgImage
        }
        iconLayer.contentsScale = 2
        iconLayer.contentsGravity = .resizeAspect
        iconLayer.frame = bounds
        layer?.addSublayer(iconLayer)
        toolTip = "Refresh usage"
    }

    required init?(coder: NSCoder) { fatalError("unused") }

    override func mouseDown(with event: NSEvent) {
        spin()
        // The menu rebuilds on fresh data; if it does not, stop after 3s so
        // the icon cannot spin forever.
        DispatchQueue.main.asyncAfter(deadline: .now() + 3) { [weak self] in
            self?.iconLayer.removeAnimation(forKey: "spin")
        }
        onClicked?()
    }

    func spin() {
        let rotation = CABasicAnimation(keyPath: "transform.rotation.z")
        rotation.fromValue = 0
        rotation.toValue = -2 * Double.pi
        rotation.duration = 0.8
        rotation.repeatCount = .infinity
        iconLayer.add(rotation, forKey: "spin")
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate, NSMenuDelegate, UNUserNotificationCenterDelegate {
    let statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    let menu = NSMenu()
    var serverProcess: Process?
    var cachedState: AppState?
    var hoverCards: [HoverCard] = []
    var hoverTimer: Timer?
    var menuOpen = false
    private var stateRequest = 0
    private var providerItems: [String: NSMenuItem] = [:]
    private var providerHovers: [String: HoverCard] = [:]
    private var countdownLabels: [(NSTextField, Double)] = []
    private var nextExhaustionChange = Date.distantFuture
    private var claudeSyncPhase = "idle"
    private var claudeSyncDetail = ""
    private var claudeSyncGeneration = 0
    private var claudeSyncConfirming = false
    private var switchingAccountID: String?
    private var nativeSwitchMessage = ""
    private var resetAlerts: [ResetAlert] = []
    private var deliveredResetIDs = Set<String>()
    private var sendingResetIDs = Set<String>()
    private var permissionRequested = false
    private var notificationPermission: ResetAlertPermission?
    private let resetLedgerURL = URL(fileURLWithPath: NSHomeDirectory() + "/.switcher/reset-alerts.json")
    private var savedResetLedger: ResetAlertLedger?

    private func persistResetAlerts() {
        let ledger = ResetAlertLedger(alerts: resetAlerts, deliveredIDs: deliveredResetIDs.sorted())
        guard ledger != savedResetLedger else { return }
        do {
            try saveResetAlertLedger(ledger, at: resetLedgerURL)
            savedResetLedger = ledger
        } catch {
            NSLog("Switcher could not save reset alerts: %@", error.localizedDescription)
        }
    }

    private func clearResetAlerts() {
        if !resetAlerts.isEmpty || !deliveredResetIDs.isEmpty || !sendingResetIDs.isEmpty {
            resetAlerts = []
            deliveredResetIDs.removeAll()
            sendingResetIDs.removeAll()
            persistResetAlerts()
        }
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .sound])
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        UNUserNotificationCenter.current().delegate = self
        ensureServerRunning()
        do {
            let ledger = try loadResetAlertLedger(at: resetLedgerURL)
            resetAlerts = ledger.alerts
            deliveredResetIDs = Set(ledger.deliveredIDs)
            savedResetLedger = ledger
        } catch {
            NSLog("Switcher could not load reset alerts: %@", error.localizedDescription)
        }
        statusItem.button?.title = "⇄"
        statusItem.button?.toolTip = "Switcher"
        menu.autoenablesItems = false
        statusItem.menu = menu
        menu.delegate = self
        rebuildMenu()
        fetchStateAsync()
        // Keep the cache warm so the menu opens instantly with recent data.
        let warm = Timer(timeInterval: 60, repeats: true) { [weak self] _ in self?.fetchStateAsync() }
        RunLoop.main.add(warm, forMode: .common)
        let alerts = Timer(timeInterval: 15, repeats: true) { [weak self] _ in self?.deliverDueResetAlerts() }
        RunLoop.main.add(alerts, forMode: .common)
    }

    // Opening draws from the cache immediately (no network on the main
    // thread), then refreshes in the background and patches the menu only if
    // the data changed. A poll drives hover while open.
    func menuWillOpen(_ menu: NSMenu) {
        menuOpen = true
        prepareMenuForOpening()
        fetchStateAsync()
        hoverTimer?.invalidate()
        let timer = Timer(timeInterval: 0.05, repeats: true) { [weak self] _ in self?.pollHover() }
        RunLoop.main.add(timer, forMode: .common)
        hoverTimer = timer
    }

    // The menu is built ahead of the click. Only time-sensitive labels need
    // attention here; expensive image rendering stays off the opening path.
    func prepareMenuForOpening(now: Date = Date()) {
        if menu.items.isEmpty || palette.dark != Palette.current().dark || now >= nextExhaustionChange {
            rebuildMenu()
            return
        }
        for (label, reset) in countdownLabels {
            guard let remaining = resetRemaining(reset) else { label.isHidden = true; continue }
            let text = "↻ " + remaining
            if textWidth(text, font: label.font!) > label.frame.width {
                rebuildMenu()
                return
            }
            label.isHidden = false
            label.stringValue = text
            label.toolTip = exactResetTime(reset)
        }
    }

    // Compare only the data the menu actually decodes. Health timestamps and
    // other web-only API fields cannot force a menu rebuild.
    func applyMenuState(_ state: AppState) {
        let previous = cachedState
        cachedState = state
        if Date() >= nextExhaustionChange { rebuildMenu(); return }
        guard previous != state else { return }
        guard let previous = previous, previous.order == state.order, previous.hidden == state.hidden,
              previous.version == state.version, previous.update == state.update,
              previous.menu_usage_bars == state.menu_usage_bars,
              previous.reset_notifications == state.reset_notifications,
              palette.dark == Palette.current().dark else { rebuildMenu(); return }
        let oldProviders = Set(previous.accounts.map(\.provider))
        let newProviders = Set(state.accounts.map(\.provider))
        guard oldProviders == newProviders else { rebuildMenu(); return }
        for providerID in providerItems.keys {
            let accounts = state.accounts.filter { $0.provider == providerID }
            if accounts == previous.accounts.filter({ $0.provider == providerID }) { continue }
            let card = providerCard(providerID: providerID, accounts: accounts,
                contentWidth: menuWidth - 2 * edgeInset, showUsageBars: state.menu_usage_bars ?? true)
            providerItems[providerID]?.view = centered(card)
            providerHovers[providerID] = card as? HoverCard
        }
        hoverCards = Array(providerHovers.values)
        // Discard references to labels in replaced provider cards.
        countdownLabels.removeAll { label, _ in
            !providerHovers.values.contains { label.isDescendant(of: $0) }
        }
        updateExhaustionDeadline()
    }

    private func updateExhaustionDeadline() {
        let now = Date()
        nextExhaustionChange = (cachedState?.accounts ?? []).compactMap { $0.exhausted_until }
            .map { Date(timeIntervalSince1970: $0) }.filter { $0 > now }.min() ?? .distantFuture
    }

    func applyNotificationPermission(_ permission: ResetAlertPermission) {
        guard notificationPermission != permission else { return }
        notificationPermission = permission
        // The prepared menu must be ready on the next click, even when the
        // permission result arrived while it was closed.
        rebuildMenu()
    }

    func menuDidClose(_ menu: NSMenu) {
        menuOpen = false
        hoverTimer?.invalidate()
        hoverTimer = nil
        hoverCards.forEach { $0.clearHover() }
    }

    func pollHover() {
        let point = NSEvent.mouseLocation
        hoverCards.forEach { $0.updateHover(screenPoint: point) }
    }

    // fetchStateAsync refreshes the cache off the main thread. The menu is
    // rebuilt only when the JSON differs from what is currently shown.
    func beginStateRequest() -> Int {
        stateRequest += 1
        return stateRequest
    }

    @discardableResult
    func acceptStateResponse(_ state: AppState, serial: Int) -> Bool {
        guard serial == stateRequest else { return false }
        applyMenuState(state)
        return true
    }

    func fetchStateAsync() {
        let serial = beginStateRequest()
        var request = URLRequest(url: hubURL.appendingPathComponent("api/state"))
        request.timeoutInterval = 5
        let token = currentDeviceToken()
        if let token = token { request.setValue("Bearer " + token, forHTTPHeaderField: "Authorization") }
        URLSession.shared.dataTask(with: request) { [weak self] data, response, _ in
            if (response as? HTTPURLResponse)?.statusCode == 401 && currentDeviceToken() != token {
                DispatchQueue.main.async { self?.fetchStateAsync() }
                return
            }
            guard let self = self, let data = data,
                  (response as? HTTPURLResponse)?.statusCode == 200,
                  let decoded = try? JSONDecoder().decode(AppState.self, from: data) else { return }
            DispatchQueue.main.async {
                guard self.acceptStateResponse(decoded, serial: serial) else { return }
                if self.menuOpen { self.prepareMenuForOpening() }
                self.updateResetAlerts()
            }
        }.resume()
    }

    // Persist upcoming and recently due windows. Nothing is pre-queued in
    // macOS, so disabling alerts still takes effect before delivery.
    func updateResetAlerts() {
        guard let state = cachedState else { return }
        if state.reset_notifications != true {
            clearResetAlerts()
            return
        }
        let now = Date()
        resetAlerts = reconciledResetAlerts(resetAlerts, state: state, now: now)
        deliveredResetIDs.formIntersection(Set(resetAlerts.map(\.id)))
        persistResetAlerts()
        let center = UNUserNotificationCenter.current()
        center.getNotificationSettings { [weak self] settings in
            DispatchQueue.main.async {
                guard let self = self else { return }
                let permission = resetAlertPermission(settings)
                self.applyNotificationPermission(permission)
                guard settings.authorizationStatus == .notDetermined,
                      self.cachedState?.reset_notifications == true, !self.permissionRequested else { return }
                self.permissionRequested = true
                center.requestAuthorization(options: [.alert, .sound]) { [weak self] _, _ in
                    center.getNotificationSettings { result in
                        DispatchQueue.main.async {
                            self?.applyNotificationPermission(resetAlertPermission(result))
                        }
                    }
                }
            }
        }
    }

    func deliverDueResetAlerts() {
        guard cachedState?.reset_notifications == true, resetAlertsEnabledOnDisk() else {
            clearResetAlerts()
            return
        }
        // The state poll refreshes this permission every minute. Do not
        // hammer the server for a due alert while macOS blocks banners.
        guard notificationPermission?.canSend == true else { return }
        let now = Date()
        let due = dueResetAlerts(resetAlerts, now: now, delivered: deliveredResetIDs, sending: sendingResetIDs)
        guard !due.isEmpty else { return }
        let serial = beginStateRequest()
        // Read the server's current preference immediately before delivery.
        // If it is unavailable, skip the alert rather than risk showing one
        // after the user turned the setting off in the web app.
        var request = URLRequest(url: hubURL.appendingPathComponent("api/state"))
        request.timeoutInterval = 5
        if let token = currentDeviceToken() { request.setValue("Bearer " + token, forHTTPHeaderField: "Authorization") }
        URLSession.shared.dataTask(with: request) { [weak self] data, response, _ in
            guard let data = data, (response as? HTTPURLResponse)?.statusCode == 200,
                  let state = try? JSONDecoder().decode(AppState.self, from: data) else { return }
            DispatchQueue.main.async {
                guard let self = self else { return }
                guard self.acceptStateResponse(state, serial: serial) else { return }
                self.updateResetAlerts()
                guard state.reset_notifications == true, resetAlertsEnabledOnDisk() else { return }
                let valid = Set(self.resetAlerts.map(\.id))
                let ready = due.filter { valid.contains($0.id) && !self.deliveredResetIDs.contains($0.id) && !self.sendingResetIDs.contains($0.id) }
                guard !ready.isEmpty else { return }
                for alert in ready { self.sendingResetIDs.insert(alert.id) }
                self.presentResetAlerts(ready)
            }
        }.resume()
    }

    private func presentResetAlerts(_ due: [ResetAlert]) {
        UNUserNotificationCenter.current().getNotificationSettings { [weak self] settings in
            DispatchQueue.main.async {
                guard let self = self else { return }
                guard resetAlertPermission(settings).canSend,
                      self.cachedState?.reset_notifications == true, resetAlertsEnabledOnDisk() else {
                    for alert in due { self.sendingResetIDs.remove(alert.id) }
                    return
                }
                for alert in due {
                    let content = UNMutableNotificationContent()
                    content.title = "\(alert.provider) usage window"
                    content.body = "Provider-reported reset time reached for \(alert.window)."
                    content.sound = .default
                    guard resetAlertsEnabledOnDisk() else { self.sendingResetIDs.remove(alert.id); continue }
                    UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: alert.id,
                        content: content, trigger: nil)) { [weak self] error in
                        DispatchQueue.main.async {
                            guard let self = self else { return }
                            self.sendingResetIDs.remove(alert.id)
                            if let error = error {
                                NSLog("Switcher reset alert delivery failed: %@", error.localizedDescription)
                            } else {
                                recordDeliveredResetAlert(alert.id, alerts: &self.resetAlerts,
                                    delivered: &self.deliveredResetIDs)
                                self.persistResetAlerts()
                            }
                        }
                    }
                }
            }
        }
    }

    // serverReachable is the one synchronous probe, used once at launch to
    // decide whether to spawn the bundled server.
    func serverReachable(timeout: Double) -> Bool {
        var request = URLRequest(url: hubURL.appendingPathComponent("api/state"))
        request.timeoutInterval = timeout
        if let token = currentDeviceToken() { request.setValue("Bearer " + token, forHTTPHeaderField: "Authorization") }
        let semaphore = DispatchSemaphore(value: 0)
        var ok = false
        URLSession.shared.dataTask(with: request) { _, response, _ in
            ok = (response as? HTTPURLResponse)?.statusCode == 200
            semaphore.signal()
        }.resume()
        _ = semaphore.wait(timeout: .now() + timeout)
        return ok
    }

    func ensureServerRunning() {
        if serverReachable(timeout: 1.5) { return }
        let server = URL(fileURLWithPath: Bundle.main.bundlePath + "/Contents/MacOS/SwitcherServer")
        guard FileManager.default.fileExists(atPath: server.path) else { return }
        let logDir = NSHomeDirectory() + "/.switcher"
        try? FileManager.default.createDirectory(atPath: logDir, withIntermediateDirectories: true)
        if !FileManager.default.fileExists(atPath: logDir + "/server.log") {
            FileManager.default.createFile(atPath: logDir + "/server.log", contents: nil)
        }
        // The log contains account emails; keep it user-only.
        try? FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: logDir + "/server.log")
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

    func post(path: String, completion: (() -> Void)? = nil) {
        var request = URLRequest(url: hubURL.appendingPathComponent(path))
        request.httpMethod = "POST"
        if let token = currentDeviceToken() { request.setValue("Bearer " + token, forHTTPHeaderField: "Authorization") }
        URLSession.shared.dataTask(with: request) { _, _, _ in completion?() }.resume()
    }

    // MARK: menu construction

    func rebuildMenu() {
        menu.removeAllItems()
        providerItems.removeAll()
        providerHovers.removeAll()
        countdownLabels.removeAll()
        updateExhaustionDeadline()
        let contentWidth = menuWidth - 2 * edgeInset

        // Header: app icon with name and version beside it, block centered.
        palette = Palette.current()
        let state = cachedState
        hoverCards = []
        let headerView = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 58))
        let nameFont = NSFont.systemFont(ofSize: 14, weight: .semibold)
        let versionFont = NSFont.systemFont(ofSize: 11)
        let versionText = "v" + (state?.version ?? "0.0.0")
        let textColumn = max(textWidth("Switcher", font: nameFont), textWidth(versionText, font: versionFont))
        let blockWidth = 36 + 10 + textColumn
        let blockX = (menuWidth - blockWidth) / 2
        let logo = NSImageView(frame: NSRect(x: blockX, y: 11, width: 36, height: 36))
        // Same SVG as the web app header, so the two marks are identical.
        logo.image = providerLogoImage("logo")
            ?? Bundle.main.image(forResource: "AppIcon")
        headerView.addSubview(logo)
        let nameField = label("Switcher", font: nameFont, color: inkColor)
        nameField.frame = NSRect(x: blockX + 46, y: 30, width: textColumn, height: 17)
        headerView.addSubview(nameField)
        let versionField = label(versionText, font: versionFont, color: dimColor)
        versionField.frame = NSRect(x: blockX + 46, y: 14, width: textColumn, height: 14)
        headerView.addSubview(versionField)
        // Manual usage refresh, trailing edge of the header.
        let refresh = SpinnerButton(symbol: "arrow.clockwise", size: 26, color: dimColor)
        refresh.frame.origin = NSPoint(x: menuWidth - edgeInset - 26, y: 16)
        refresh.onClicked = { [weak self] in self?.refreshUsage() }
        headerView.addSubview(refresh)
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
                menu.addItem(menuItemWithView(sectionHead(providerID, contentWidth: contentWidth,
                    syncPhase: claudeSyncPhase, onSync: { [weak self] in self?.syncClaudeSessions() })))
                if providerID == "claude", !nativeSwitchMessage.isEmpty {
                    let row = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 40))
                    let note = label(nativeSwitchMessage, font: NSFont.systemFont(ofSize: 11), color: dimColor)
                    note.maximumNumberOfLines = 2
                    note.frame = NSRect(x: edgeInset, y: 5, width: contentWidth, height: 32)
                    note.toolTip = nativeSwitchMessage
                    row.addSubview(note)
                    menu.addItem(menuItemWithView(row))
                }
                if providerID == "claude" && (claudeSyncPhase == "success" || claudeSyncPhase == "error") {
                    let feedback = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 48))
                    if claudeSyncPhase == "error" {
                        let details = NSButton(title: "Sync failed · Details", target: self, action: #selector(showClaudeSyncError))
                        details.isBordered = false
                        details.contentTintColor = .systemRed
                        details.frame = NSRect(x: edgeInset, y: 10, width: contentWidth, height: 26)
                        feedback.addSubview(details)
                    } else {
                        let text = label("Claude sessions synced\n" + claudeSyncDetail,
                            font: NSFont.systemFont(ofSize: 11), color: dimColor)
                        text.maximumNumberOfLines = 2
                        text.frame = NSRect(x: edgeInset, y: 5, width: contentWidth, height: 38)
                        text.toolTip = claudeSyncDetail
                        feedback.addSubview(text)
                    }
                    menu.addItem(menuItemWithView(feedback))
                }
                let card = providerCard(providerID: providerID, accounts: accounts,
                    contentWidth: contentWidth, showUsageBars: state?.menu_usage_bars ?? true)
                let item = menuItemWithView(centered(card))
                providerItems[providerID] = item
                providerHovers[providerID] = card as? HoverCard
                menu.addItem(item)
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
        webButton.attributedTitle = NSAttributedString(string: webButton.title, attributes: [
            .font: NSFont.systemFont(ofSize: 13, weight: .medium), .foregroundColor: inkColor,
            .paragraphStyle: centeredParagraph])
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
        updateButton.attributedTitle = NSAttributedString(string: updateButton.title, attributes: [
            .font: NSFont.systemFont(ofSize: 13, weight: .medium), .foregroundColor: inkColor,
            .paragraphStyle: centeredParagraph])
        updateCard.addSubview(updateButton)
        actionRow.addSubview(updateCard)
        menu.addItem(menuItemWithView(actionRow))

        // Start at login: symmetric card, label left, toggle right.
        let toggleCard = cardView(width: contentWidth, height: 42)
        let field = label("Start at login", font: NSFont.systemFont(ofSize: 13), color: inkColor)
        field.frame = NSRect(x: cardInset, y: 13, width: 180, height: 16)
        toggleCard.addSubview(field)
        let toggle = ToggleSwitch(isOn: startAtLoginEnabled())
        toggle.frame.origin = NSPoint(x: contentWidth - cardInset - 40, y: 9)
        toggle.onChanged = { [weak self] _ in self?.toggleStartAtLogin() }
        toggleCard.addSubview(toggle)
        menu.addItem(menuItemWithView(centered(toggleCard, verticalPadding: 4)))

        if state?.reset_notifications == true {
            let statusItem = NSMenuItem(title: notificationPermission?.message ?? "Checking notification permission",
                action: nil, keyEquivalent: "")
            statusItem.isEnabled = false
            menu.addItem(statusItem)
            let testItem = NSMenuItem(title: "Send test reset alert", action: #selector(testResetAlert), keyEquivalent: "")
            testItem.target = self
            testItem.isEnabled = notificationPermission?.canSend == true
            menu.addItem(testItem)
        }

        let quitItem = NSMenuItem(title: "Quit Switcher", action: #selector(quit), keyEquivalent: "q")
        quitItem.target = self
        menu.addItem(quitItem)
    }

    // Row metrics: top pad, title, gap, usage lines, bottom pad.
    let rowTopPad: CGFloat = 9
    let titleHeight: CGFloat = 15
    let titleUsageGap: CGFloat = 4
    let usageLineHeight: CGFloat = 15
    let rowBottomPad: CGFloat = 8

    func providerCard(providerID: String, accounts: [Account], contentWidth: CGFloat,
                      showUsageBars: Bool) -> NSView {
        let height = accounts.reduce(CGFloat(0)) { $0 + rowHeight(for: $1) }
        let card = cardView(width: contentWidth, height: height)
        if let hover = card as? HoverCard { hoverCards.append(hover) }
        // Column widths shared by every row in this card (measured with the
        // medium weight, the wider of the two title weights).
        let measureFont = NSFont.systemFont(ofSize: 12.5, weight: .medium)
        let planColumn = accounts
            .compactMap { $0.plan.flatMap { planNames[$0] } }
            .map { textWidth("· " + $0, font: measureFont) }.max() ?? 0
        let emailColumn = accounts.map { textWidth($0.email, font: measureFont) }.max() ?? 0
        var y = height
        for (index, account) in accounts.enumerated() {
            let rowH = rowHeight(for: account)
            y -= rowH

            let row = ClickableRow(frame: NSRect(x: 0, y: y, width: contentWidth, height: rowH))
            row.onClicked = { [weak self] in
                self?.activateAccount(account)
            }

            // Email + plan. Plans sit in one column per card: the email
            // column is as wide as the widest email that still leaves room
            // for the widest plan; longer emails truncate with an ellipsis.
            let textX = cardInset
            let planText = account.plan.flatMap { planNames[$0] }.map { "· " + $0 } ?? ""
            let bankedText = bankedResetText(account.reset_credits) ?? ""
            let bankedFont = NSFont.systemFont(ofSize: 10.5)
            let bankedWidth = bankedText.isEmpty ? CGFloat(0) : textWidth(bankedText, font: bankedFont)
            let exhausted = !account.selected && (account.exhausted_until.map { $0 > Date().timeIntervalSince1970 } ?? false)
            let titleFont = NSFont.systemFont(ofSize: 12.5, weight: account.selected ? .medium : .regular)
            let titleColor = account.selected ? accentColor : inkColor
            let trailingWidth = max(account.selected ? 14 : 0, exhausted ? 90 : 0) +
                (bankedWidth > 0 ? bankedWidth + 8 : 0)
            let titleWidth = contentWidth - textX - cardInset - trailingWidth - 8
            let titleY = rowH - rowTopPad - titleHeight
            let emailWidth = planColumn > 0 ? max(0, min(emailColumn, titleWidth - planColumn - 6)) : max(0, min(emailColumn, titleWidth))
            let emailField = label(account.email, font: titleFont, color: titleColor)
            emailField.frame = NSRect(x: textX, y: titleY, width: emailWidth, height: titleHeight)
            let email = BlurredLabel(field: emailField)
            row.addSubview(email)
            row.onHover = { hovering in email.reveal(hovering) }
            if !planText.isEmpty {
                let plan = label(planText, font: titleFont, color: titleColor)
                plan.frame = NSRect(x: textX + emailWidth + 6, y: titleY, width: planColumn, height: titleHeight)
                row.addSubview(plan)
            }
            if bankedWidth > 0 {
                let banked = label(bankedText, font: bankedFont, color: warnColor)
                banked.alignment = .right
                banked.toolTip = "Banked usage-limit resets available"
                banked.frame = NSRect(x: contentWidth - cardInset - (account.selected ? 22 : 0) - bankedWidth,
                                      y: titleY, width: bankedWidth, height: titleHeight)
                row.addSubview(banked)
            }

            // Stable label and percentage columns, with resets trailing.
            let usageFont = NSFont.systemFont(ofSize: 11)
            let valueFont = NSFont.monospacedDigitSystemFont(ofSize: 11, weight: .regular)
            let resetFont = NSFont.systemFont(ofSize: 10.5)
            let windows = account.usage?.windows ?? []
            let layout = usageLayout(windows: windows, titleY: titleY,
                lineWidth: contentWidth - textX - cardInset, contentWidth: contentWidth, textX: textX,
                cardInset: cardInset, lineHeight: usageLineHeight, titleGap: titleUsageGap,
                showBars: showUsageBars)
            for (window, line) in zip(windows, layout.lines) {
                let left = max(0, min(100, 100 - window.used_percent))
                if let barFrame = line.bar {
                    let track = NSView(frame: barFrame)
                    track.wantsLayer = true
                    track.layer?.backgroundColor = dimColor.withAlphaComponent(0.24).cgColor
                    track.layer?.cornerRadius = 1.5
                    track.layer?.masksToBounds = true
                    if left > 0 {
                        let fill = NSView(frame: NSRect(x: 0, y: 0,
                            width: barFrame.width * CGFloat(left) / 100, height: barFrame.height))
                        fill.wantsLayer = true
                        fill.layer?.backgroundColor = accentColor.cgColor
                        fill.layer?.cornerRadius = 1.5
                        track.addSubview(fill)
                    }
                    row.addSubview(track)
                }
                let name = label(window.label + ":", font: usageFont, color: dimColor)
                name.frame = line.label
                row.addSubview(name)
                let value = label("\(left)% left", font: valueFont, color: dimColor)
                value.frame = line.value
                row.addSubview(value)
                if let reset = line.resetText, let frame = line.reset {
                    let resetLabel = label(reset, font: resetFont, color: dimColor)
                    resetLabel.toolTip = exactResetTime(window.resets_at)
                    if let timestamp = window.resets_at { countdownLabels.append((resetLabel, timestamp)) }
                    resetLabel.alignment = .right
                    resetLabel.frame = frame
                    row.addSubview(resetLabel)
                }
            }

            if account.selected {
                let check = NSImageView(frame: layout.checkmark)
                check.image = NSImage(systemSymbolName: "checkmark", accessibilityDescription: nil)
                check.contentTintColor = accentColor
                row.addSubview(check)
            } else if exhausted {
                let badge = label("Out of usage", font: NSFont.systemFont(ofSize: 10.5), color: warnColor)
                badge.alignment = .right
                badge.frame = NSRect(x: contentWidth - cardInset - 90 - (bankedWidth > 0 ? bankedWidth + 8 : 0),
                                     y: titleY, width: 90, height: 14)
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
        let usageBlock = windows > 0 ? titleUsageGap + windows * usageLineHeight : 0
        return rowTopPad + titleHeight + usageBlock + rowBottomPad
    }

    // MARK: actions

    func activateAccount(_ account: Account) {
        guard switchingAccountID == nil else { return }
        switchingAccountID = account.id
        if account.native_switch_available == true {
            nativeSwitchMessage = "Switching Claude Code login…"
            rebuildMenu()
        }
        var request = URLRequest(url: hubURL.appendingPathComponent("api/accounts/\(account.id)/activate"))
        request.httpMethod = "POST"
        request.timeoutInterval = 90
        if let token = currentDeviceToken() { request.setValue("Bearer " + token, forHTTPHeaderField: "Authorization") }
        URLSession.shared.dataTask(with: request) { [weak self] data, response, error in
            let body = data.flatMap { try? JSONDecoder().decode(AccountActivationResponse.self, from: $0) }
            let ok = (response as? HTTPURLResponse)?.statusCode == 200
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.switchingAccountID = nil
                self.nativeSwitchMessage = ok ? (body?.native?.message ?? "") : ""
                self.rebuildMenu()
                self.fetchStateAsync()
                if !ok {
                    let alert = NSAlert()
                    alert.messageText = body?.error ?? "Could not switch account"
                    alert.informativeText = [body?.details, body?.backup.map { "Backup: " + $0 }, error?.localizedDescription]
                        .compactMap { $0 }.joined(separator: "\n")
                    alert.addButton(withTitle: "OK")
                    alert.runModal()
                } else if !self.nativeSwitchMessage.isEmpty {
                    let message = self.nativeSwitchMessage
                    DispatchQueue.main.asyncAfter(deadline: .now() + 8) { [weak self] in
                        guard self?.nativeSwitchMessage == message else { return }
                        self?.nativeSwitchMessage = ""
                        self?.rebuildMenu()
                    }
                }
            }
        }.resume()
    }

    func syncClaudeSessions() {
        guard claudeSyncPhase != "running", !claudeSyncConfirming else { return }
        claudeSyncConfirming = true
        menu.cancelTracking()
        NSApp.activate(ignoringOtherApps: true)
        let confirmed = claudeSyncConfirmation().runModal() == .alertSecondButtonReturn
        claudeSyncConfirming = false
        guard confirmed else { return }
        claudeSyncGeneration += 1
        let generation = claudeSyncGeneration
        claudeSyncPhase = "running"
        claudeSyncDetail = ""
        rebuildMenu()
        var request = URLRequest(url: hubURL.appendingPathComponent("api/claude/sync"))
        request.httpMethod = "POST"
        request.timeoutInterval = 90
        if let token = currentDeviceToken() { request.setValue("Bearer " + token, forHTTPHeaderField: "Authorization") }
        URLSession.shared.dataTask(with: request) { [weak self] data, response, error in
            let body = data.flatMap { try? JSONDecoder().decode(ClaudeSyncResponse.self, from: $0) }
            let ok = (response as? HTTPURLResponse)?.statusCode == 200 && body?.detail != nil
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.claudeSyncPhase = ok ? "success" : "error"
                self.claudeSyncDetail = ok ? (body?.detail ?? "No new sessions to share") :
                    [body?.error, body?.details, error?.localizedDescription,
                     body?.result?.backup.map { "Backup: " + $0 }].compactMap { $0 }.joined(separator: "\n")
                if self.claudeSyncDetail.isEmpty { self.claudeSyncDetail = "Could not reach Switcher. Try again." }
                self.rebuildMenu()
                if ok {
                    DispatchQueue.main.asyncAfter(deadline: .now() + 6) { [weak self] in
                        guard self?.claudeSyncPhase == "success", self?.claudeSyncGeneration == generation else { return }
                        self?.claudeSyncPhase = "idle"
                        self?.rebuildMenu()
                    }
                }
            }
        }.resume()
    }

    @objc func showClaudeSyncError() {
        let alert = NSAlert()
        alert.messageText = "Claude session sync did not complete"
        alert.informativeText = claudeSyncDetail
        alert.addButton(withTitle: "OK")
        alert.runModal()
    }

    @objc func refreshUsage() {
        post(path: "api/usage/refresh") { [weak self] in
            DispatchQueue.main.async { self?.fetchStateAsync() }
        }
    }

    @objc func openWebApp() {
        NSWorkspace.shared.open(hubURL)
    }

    @objc func runUpdate() {
        post(path: "api/update") { [weak self] in
            // The server exec-restarts; give it a moment before re-reading.
            DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) { self?.fetchStateAsync() }
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
            if let data = try? PropertyListSerialization.data(fromPropertyList: plist, format: .xml, options: 0) {
                try? data.write(to: URL(fileURLWithPath: launchAgentPath))
            }
        }
    }

    @objc func testResetAlert() {
        guard cachedState?.reset_notifications == true else { return }
        sendTestResetAlert { error in
            if let error = error { NSLog("Switcher test alert failed: %@", error) }
        }
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
    NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: height))
}

// centered wraps a card in a full menu-width container so NSMenu cannot
// pin it to the left edge. Equal edgeInset on both sides.
func centered(_ card: NSView, verticalPadding: CGFloat = 0) -> NSView {
    let container = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: card.frame.height + 2 * verticalPadding))
    card.frame.origin = NSPoint(x: (menuWidth - card.frame.width) / 2, y: verticalPadding)
    container.addSubview(card)
    return container
}

#if !SWITCHER_LAYOUT_TEST
@main
struct SwitcherApp {
    static func main() {
        if CommandLine.arguments.dropFirst() == ["--notification-status"] {
            let done = DispatchSemaphore(value: 0)
            UNUserNotificationCenter.current().getNotificationSettings { settings in
                let status = resetAlertPermission(settings)
                FileHandle.standardOutput.write(Data((status.message + "\n").utf8))
                done.signal()
            }
            guard done.wait(timeout: .now() + 5) == .success else {
                FileHandle.standardError.write(Data("Could not read macOS notification settings\n".utf8))
                exit(1)
            }
            return
        }
        if CommandLine.arguments.dropFirst() == ["--test-notification"] {
            let done = DispatchSemaphore(value: 0)
            sendTestResetAlert { error in
                if let error = error {
                    FileHandle.standardError.write(Data((error + "\n").utf8))
                    exit(1)
                }
                FileHandle.standardOutput.write(Data("Reset test alert accepted by macOS\n".utf8))
                done.signal()
            }
            guard done.wait(timeout: .now() + 5) == .success else {
                FileHandle.standardError.write(Data("Reset test alert timed out\n".utf8))
                exit(1)
            }
            return
        }
        let app = NSApplication.shared
        let delegate = AppDelegate()
        app.delegate = delegate
        app.setActivationPolicy(.accessory) // menu bar app: no Dock icon
        app.run()
    }
}
#endif
