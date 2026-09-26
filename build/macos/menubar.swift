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

struct UsageWindow: Codable {
    let label: String
    let used_percent: Int
    let resets_at: Double?
}

struct Usage: Codable {
    let available: Bool
    let windows: [UsageWindow]?
}

struct ResetCredits: Codable {
    let count: Int
}

func bankedResetText(_ credits: ResetCredits?) -> String? {
    guard let count = credits?.count, count > 0 else { return nil }
    return "⚡ \(count) banked"
}

struct Account: Codable {
    let id: String
    let provider: String
    let email: String
    let plan: String?
    let active: Bool
    let exhausted_until: Double?
    let usage: Usage?
    let reset_credits: ResetCredits?
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
    let menu_usage_bars: Bool?
    let reset_notifications: Bool?
}

private let resetPrefix = "sh.switcher.reset."

struct ResetAlert {
    let id: String
    let provider: String
    let window: String
    let date: Date
}

func desiredResetAlerts(_ state: AppState, now: Date = Date()) -> [ResetAlert] {
    guard state.reset_notifications == true else { return [] }
    let end = now.addingTimeInterval(30 * 86400)
    var alerts: [ResetAlert] = []
    for account in state.accounts where account.usage?.available == true {
        for (index, window) in (account.usage?.windows ?? []).enumerated() {
            guard window.used_percent > 0, let seconds = window.resets_at else { continue }
            let date = Date(timeIntervalSince1970: seconds)
            guard date > now && date <= end else { continue }
            alerts.append(ResetAlert(id: resetPrefix + account.id + ".\(index).\(Int(seconds))",
                provider: providerNames[account.provider] ?? account.provider, window: window.label, date: date))
        }
    }
    return Array(alerts.sorted { $0.date == $1.date ? $0.id < $1.id : $0.date < $1.date }.prefix(32))
}

let providerNames = ["codex": "Codex", "claude": "Claude", "grok": "Grok", "opencode": "OpenCode",
    "antigravity": "Antigravity", "gemini": "Gemini", "copilot": "Copilot"]
let planNames = ["max": "Max 20x", "pro": "Pro 20x", "prolite": "Pro 5x", "plus": "Plus", "free": "Free",
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

func sectionHead(_ providerID: String, contentWidth: CGFloat) -> NSView {
    let head = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 32))
    let logo = NSImageView(frame: NSRect(x: edgeInset, y: 5, width: 22, height: 22))
    logo.imageScaling = .scaleProportionallyUpOrDown
    logo.image = providerLogoImage(providerID)
    logo.contentTintColor = inkColor
    let name = label(providerNames[providerID] ?? providerID,
        font: NSFont.systemFont(ofSize: 13, weight: .semibold), color: inkColor)
    name.frame = NSRect(x: edgeInset + 22 + 10, y: 8, width: contentWidth - 32, height: 16)
    head.addSubview(logo)
    head.addSubview(name)
    return head
}

// Copilot's single-path mark and Grok's glyph follow the current palette;
// other provider marks retain their own colours.
func logoUsesTemplate(_ providerID: String) -> Bool {
    providerID == "grok" || providerID == "copilot"
}

// providerLogoImage loads the same provider marks used by the web app.
func providerLogoImage(_ providerID: String) -> NSImage? {
    guard let url = Bundle.main.url(forResource: providerID, withExtension: "svg") else {
        return nil
    }
    guard let image = NSImage(contentsOf: url) else { return nil }
    image.isTemplate = logoUsesTemplate(providerID)
    if providerID == "opencode" && palette.dark {
        return invertedImage(image, size: NSSize(width: 22, height: 22)) ?? image
    }
    return image
}

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
          let result = CIContext().createCGImage(masked, from: input.extent) else { return nil }
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
    let formatter = DateFormatter()
    formatter.dateStyle = .full
    formatter.timeStyle = .full
    formatter.timeZone = .current
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
    let probe = NSTextField(labelWithString: text)
    probe.font = font
    probe.sizeToFit()
    return ceil(probe.frame.width) + 2
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
    guard let rep = renderBitmap(field), let cg = rep.cgImage else { return nil }
    let input = CIImage(cgImage: cg)
    guard let clamp = CIFilter(name: "CIAffineClamp"), let blur = CIFilter(name: "CIGaussianBlur") else { return nil }
    clamp.setValue(input, forKey: kCIInputImageKey)
    clamp.setValue(CGAffineTransform.identity, forKey: kCIInputTransformKey)
    blur.setValue(clamp.outputImage, forKey: kCIInputImageKey)
    blur.setValue(blurRadius * 2, forKey: kCIInputRadiusKey) // radius in 2x pixels
    guard let output = blur.outputImage?.cropped(to: input.extent),
          let result = CIContext().createCGImage(output, from: input.extent) else { return nil }
    let image = NSImage(cgImage: result, size: field.bounds.size)
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
    var cachedRaw: Data?
    var hoverCards: [HoverCard] = []
    var hoverTimer: Timer?
    var menuOpen = false
    private var stateRequest = 0
    private var resetAlerts: [ResetAlert] = []
    private var deliveredResetIDs = Set<String>()
    private var sendingResetIDs = Set<String>()
    private var permissionRequested = false

    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .sound])
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        UNUserNotificationCenter.current().delegate = self
        ensureServerRunning()
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
        rebuildMenu()
        fetchStateAsync()
        hoverTimer?.invalidate()
        let timer = Timer(timeInterval: 0.05, repeats: true) { [weak self] _ in self?.pollHover() }
        RunLoop.main.add(timer, forMode: .common)
        hoverTimer = timer
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
    func fetchStateAsync() {
        stateRequest += 1
        let serial = stateRequest
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
                guard serial >= self.stateRequest else { return }
                let changed = self.cachedRaw != data
                self.cachedState = decoded
                self.cachedRaw = data
                if changed { self.rebuildMenu() }
                self.updateResetAlerts()
            }
        }.resume()
    }

    // Keep only upcoming windows in memory. Nothing remains queued in macOS
    // after the app closes or the web preference is turned off.
    func updateResetAlerts() {
        guard let state = cachedState else { return }
        if state.reset_notifications != true {
            resetAlerts = []
            deliveredResetIDs.removeAll()
            return
        }
        let now = Date()
        let upcoming = desiredResetAlerts(state, now: now)
        // Preserve a just-due window until the delivery timer gets to it,
        // unless its account or window vanished from the newest snapshot.
        let currentIDs = Set(state.accounts.flatMap { account in
            (account.usage?.windows ?? []).enumerated().compactMap { index, window -> String? in
                guard account.usage?.available == true, window.used_percent > 0,
                      let seconds = window.resets_at else { return nil }
                return resetPrefix + account.id + ".\(index).\(Int(seconds))"
            }
        })
        let due = resetAlerts.filter { $0.date <= now && now.timeIntervalSince($0.date) < 75 && currentIDs.contains($0.id) }
        resetAlerts = due + upcoming
        let center = UNUserNotificationCenter.current()
        center.getNotificationSettings { [weak self] settings in
            guard settings.authorizationStatus == .notDetermined else { return }
            DispatchQueue.main.async {
                guard let self = self, self.cachedState?.reset_notifications == true,
                      !self.permissionRequested else { return }
                self.permissionRequested = true
                center.requestAuthorization(options: [.alert, .sound]) { _, _ in }
            }
        }
    }

    func deliverDueResetAlerts() {
        guard cachedState?.reset_notifications == true, resetAlertsEnabledOnDisk() else { resetAlerts = []; return }
        let now = Date()
        let due = resetAlerts.filter { $0.date <= now && now.timeIntervalSince($0.date) < 75 && !deliveredResetIDs.contains($0.id) && !sendingResetIDs.contains($0.id) }
        guard !due.isEmpty else { return }
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
                self.cachedState = state
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
                guard settings.authorizationStatus == .authorized || settings.authorizationStatus == .provisional,
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
                            self?.sendingResetIDs.remove(alert.id)
                            if error == nil { self?.deliveredResetIDs.insert(alert.id) }
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
        logo.image = Bundle.main.url(forResource: "logo", withExtension: "svg").flatMap { NSImage(contentsOf: $0) }
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
                menu.addItem(menuItemWithView(sectionHead(providerID, contentWidth: contentWidth)))
                let card = providerCard(providerID: providerID, accounts: accounts,
                    contentWidth: contentWidth, showUsageBars: state?.menu_usage_bars ?? true)
                menu.addItem(menuItemWithView(centered(card)))
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
                self?.post(path: "api/accounts/\(account.id)/activate") { [weak self] in
                    DispatchQueue.main.async { self?.fetchStateAsync() }
                }
            }

            // Email + plan. Plans sit in one column per card: the email
            // column is as wide as the widest email that still leaves room
            // for the widest plan; longer emails truncate with an ellipsis.
            let textX = cardInset
            let planText = account.plan.flatMap { planNames[$0] }.map { "· " + $0 } ?? ""
            let bankedText = bankedResetText(account.reset_credits) ?? ""
            let bankedFont = NSFont.systemFont(ofSize: 10.5)
            let bankedWidth = bankedText.isEmpty ? CGFloat(0) : textWidth(bankedText, font: bankedFont)
            let exhausted = !account.active && (account.exhausted_until.map { $0 > Date().timeIntervalSince1970 } ?? false)
            let titleFont = NSFont.systemFont(ofSize: 12.5, weight: account.active ? .medium : .regular)
            let titleColor = account.active ? accentColor : inkColor
            let trailingWidth = max(account.active ? 14 : 0, exhausted ? 90 : 0) +
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
                banked.frame = NSRect(x: contentWidth - cardInset - (account.active ? 22 : 0) - bankedWidth,
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
                    resetLabel.alignment = .right
                    resetLabel.frame = frame
                    row.addSubview(resetLabel)
                }
            }

            if account.active {
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
        let app = NSApplication.shared
        let delegate = AppDelegate()
        app.delegate = delegate
        app.setActivationPolicy(.accessory) // menu bar app: no Dock icon
        app.run()
    }
}
#endif
