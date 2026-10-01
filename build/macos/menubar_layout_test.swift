import AppKit
import UserNotifications

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(1)
}

@main
struct MenuBarLayoutTest {
    static func main() {
        _ = NSApplication.shared
        guard planNames["claude_max_5x"] == "Max 5x",
              planNames["claude_max_20x"] == "Max 20x",
              planNames["claude_max"] == "Max" else { fail("Claude Max tiers must not be inferred from a generic max flag") }
        for phase in ["idle", "running", "success", "error"] {
            let head = sectionHead("claude", contentWidth: menuWidth - 2 * edgeInset, syncPhase: phase)
            guard let button = head.subviews.compactMap({ $0 as? ClaudeSyncButton }).first,
                  button.isEnabled == (phase != "running"),
                  button.toolTip?.contains("Sync Claude Code sessions between accounts") == true else {
                fail("Claude sync button missing or has incorrect state")
            }
            let labels = head.subviews.compactMap { $0 as? NSTextField }
            guard labels.allSatisfy({ !$0.frame.intersects(button.frame) && $0.frame.maxX < button.frame.minX }) else { fail("Claude sync button must follow its title without overlapping") }
        }
        let syncConfirmation = claudeSyncConfirmation()
        guard syncConfirmation.messageText == "Sync Claude Code sessions?",
              syncConfirmation.informativeText.contains("close and reopen Claude Desktop"),
              syncConfirmation.informativeText.contains("background"),
              syncConfirmation.buttons.map(\.title) == ["Cancel", "Sync sessions"],
              syncConfirmation.buttons[0].keyEquivalent == "\r",
              syncConfirmation.buttons[1].keyEquivalent.isEmpty else { fail("Sync confirmation must explain interruption and default to Cancel") }
        guard !sectionHead("codex", contentWidth: menuWidth - 2 * edgeInset).subviews.contains(where: { $0 is ClaudeSyncButton }) else {
            fail("Claude sync action appeared on another provider")
        }
        let now = Date().timeIntervalSince1970
        var cases: [[UsageWindow]] = [
            [UsageWindow(label: "Weekly", used_percent: 3,
                resets_at: now + 5 * 86400 + 3 * 3600)],
            [
                UsageWindow(label: "Go · Session", used_percent: 4,
                    resets_at: now + 4 * 3600),
                UsageWindow(label: "Go · Weekly", used_percent: 47,
                    resets_at: now + 3 * 86400 + 2 * 3600),
                UsageWindow(label: "Go · Monthly", used_percent: 65,
                    resets_at: now + 12 * 86400 + 8 * 3600),
            ],
        ]
        if let raw = ProcessInfo.processInfo.environment["SWITCHER_USAGE_WINDOWS"],
           let data = raw.data(using: .utf8),
           let live = try? JSONDecoder().decode([[UsageWindow]].self, from: data) {
            cases.append(contentsOf: live)
        }
        let contentWidth = menuWidth - 2 * edgeInset
        for windows in cases {
            let lineHeight: CGFloat = 15
            let rowHeight = CGFloat(9 + 15 + 4 + 8) + CGFloat(windows.count) * lineHeight
            let titleY = rowHeight - 9 - 15
            let textX = cardInset
            for showBars in [true, false] {
                let layout = usageLayout(windows: windows, titleY: titleY,
                    lineWidth: contentWidth - textX - cardInset,
                    contentWidth: contentWidth, textX: textX,
                    cardInset: cardInset, lineHeight: lineHeight, titleGap: 4,
                    showBars: showBars)
                for line in layout.lines {
                    guard line.value.width >= textWidth("100% left", font: NSFont.monospacedDigitSystemFont(ofSize: 11, weight: .regular)) else {
                        fail("Percentage value clipped")
                    }
                    guard !line.label.intersects(layout.checkmark) && !line.value.intersects(layout.checkmark) else {
                        fail("Checkmark overlaps a usage line")
                    }
                    if let reset = line.resetText, let frame = line.reset {
                        let needed = textWidth(reset, font: NSFont.systemFont(ofSize: 10.5))
                        guard frame.width >= needed else {
                            fail("Truncated reset: \(reset), need \(needed), got \(frame.width)")
                        }
                        guard frame.minX >= line.value.maxX + 5 && frame.maxX <= contentWidth - cardInset else {
                            fail("Reset overlaps value or escapes card inset")
                        }
                        guard !frame.intersects(layout.checkmark) else {
                            fail("Reset overlaps active checkmark")
                        }
                    } else if line.resetText != nil || line.reset != nil {
                        fail("Reset text and frame disagree")
                    }
                    if showBars {
                        guard let bar = line.bar, bar.minX >= line.label.maxX + 5,
                              line.value.minX >= bar.maxX + 5,
                              bar.maxY <= line.label.maxY else {
                            fail("Inline usage bar overlaps a label or percentage")
                        }
                    } else if line.bar != nil {
                        fail("Usage bar present when disabled")
                    }
                }
                guard layout.checkmark.minY >= titleY && layout.checkmark.maxY <= titleY + 15 else {
                    fail("Checkmark is outside the account title line")
                }
            }
        }
        let noReset = usageLayout(windows: [UsageWindow(label: "Fable", used_percent: 0, resets_at: nil)],
            titleY: 27, lineWidth: contentWidth - 2 * cardInset, contentWidth: contentWidth,
            textX: cardInset, cardInset: cardInset, lineHeight: 15, titleGap: 4, showBars: true)
        guard noReset.lines[0].reset == nil && noReset.lines[0].resetText == nil else {
            fail("A window without a reset timestamp acquired a countdown")
        }
        guard exactResetTime(nil) == nil, exactResetTime(now - 10) == nil,
              let exact = exactResetTime(now + 3600), exact.contains("Provider-reported reset"),
              exact.contains(" (UTC") else {
            fail("Exact reset hover lacks local time zone or appears for an absent reset")
        }
        let longLabel = usageLayout(windows: [UsageWindow(
            label: "Very long window label from upstream", used_percent: 0,
            resets_at: now + 12 * 86400)], titleY: 27,
            lineWidth: contentWidth - 2 * cardInset, contentWidth: contentWidth,
            textX: cardInset, cardInset: cardInset, lineHeight: 15, titleGap: 4,
            showBars: true)
        guard longLabel.lines[0].bar == nil &&
              longLabel.lines[0].value.width >= textWidth("100% left", font: NSFont.monospacedDigitSystemFont(ofSize: 11, weight: .regular)) else {
            fail("Long label did not preserve the percentage column")
        }
        let base = Account(id: "a", provider: "codex", email: "private@example.com", plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: true, windows: [
                UsageWindow(label: "Session", used_percent: 25, resets_at: now + 3600),
                UsageWindow(label: "Weekly", used_percent: 0, resets_at: now + 7200),
                UsageWindow(label: "Past", used_percent: 50, resets_at: now - 5),
                UsageWindow(label: "Unknown", used_percent: 50, resets_at: nil),
            ]), reset_credits: ResetCredits(count: 1))
        var nativeAccount = base
        nativeAccount.native_switch_available = true
        nativeAccount.native_active = false
        guard !nativeAccount.selected else { fail("Proxy selection was mislabeled as a native login") }
        nativeAccount.native_active = true
        guard nativeAccount.selected else { fail("Observed native selection was not displayed") }
        guard bankedResetText(base.reset_credits) == "⚡ 1 banked",
              bankedResetText(nil) == nil,
              bankedResetText(ResetCredits(count: 0)) == nil else {
            fail("Banked reset count missing or shown for an empty balance")
        }
        func state(_ accounts: [Account], _ enabled: Bool) -> AppState {
            AppState(accounts: accounts, order: nil, hidden: nil, version: nil,
                update: nil, menu_usage_bars: nil, reset_notifications: enabled)
        }
        // A click reuses the prepared menu. Updating one provider preserves
        // the other provider's views and hover targets.
        let otherProvider = Account(id: "claude-fixture", provider: "claude", email: "fixture@example.test",
            plan: "claude_max_5x", active: false, exhausted_until: nil, usage: base.usage, reset_credits: nil)
        let delegate = AppDelegate()
        let initial = state([base, otherProvider], false)
        delegate.applyMenuState(initial)
        let items = delegate.menu.items.map(ObjectIdentifier.init)
        func providerViews() -> [NSView] {
            delegate.menu.items.compactMap(\.view).filter { container in
                container.subviews.contains { $0 is HoverCard && $0.subviews.contains(where: { $0 is ClickableRow }) }
            }
        }
        let cards = providerViews()
        guard cards.count == 2 else { fail("Missing provider fixture cards") }
        for _ in 0..<10 { delegate.prepareMenuForOpening() }
        delegate.applyMenuState(initial)
        guard delegate.menu.items.map(ObjectIdentifier.init) == items else { fail("Unchanged menu was rebuilt on open or poll") }
        let changedAccount = Account(id: base.id, provider: base.provider, email: base.email, plan: base.plan,
            active: base.active, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Session", used_percent: 7, resets_at: now + 3600)]), reset_credits: nil)
        delegate.applyMenuState(state([changedAccount, otherProvider], false))
        let nextCards = providerViews()
        guard nextCards[0] !== cards[0], nextCards[1] === cards[1] else { fail("Provider patch rebuilt unrelated cards") }
        delegate.applyMenuState(state([changedAccount, otherProvider], true))
        delegate.applyNotificationPermission(ResetAlertPermission(message: "Fixture permission granted", canSend: true))
        guard delegate.menu.items.contains(where: { $0.title == "Fixture permission granted" }),
              delegate.menu.items.contains(where: { $0.title == "Send test reset alert" && $0.isEnabled }) else {
            fail("Permission change while closed left stale prepared controls")
        }
        let expiry = Date().timeIntervalSince1970 + 1
        let parked = Account(id: base.id, provider: base.provider, email: base.email, plan: base.plan,
            active: false, exhausted_until: expiry, usage: base.usage, reset_credits: nil)
        delegate.applyMenuState(state([parked, otherProvider], false))
        Thread.sleep(forTimeInterval: max(0, expiry - Date().timeIntervalSince1970 + 0.02))
        let changedOther = Account(id: otherProvider.id, provider: otherProvider.provider, email: otherProvider.email,
            plan: "claude_max_20x", active: false, exhausted_until: nil, usage: otherProvider.usage, reset_credits: nil)
        delegate.applyMenuState(state([parked, changedOther], false))
        func hasExpiredBadge(_ view: NSView) -> Bool {
            if let field = view as? NSTextField, field.stringValue == "Out of usage" { return true }
            return view.subviews.contains(where: hasExpiredBadge)
        }
        guard !providerViews().contains(where: hasExpiredBadge) else { fail("Unrelated provider patch lost an expired exhaustion deadline") }
        let olderPoll = delegate.beginStateRequest()
        let newerNotificationCheck = delegate.beginStateRequest()
        guard delegate.acceptStateResponse(initial, serial: newerNotificationCheck),
              !delegate.acceptStateResponse(state([], false), serial: olderPoll),
              delegate.cachedState == initial else { fail("Old poll replaced a newer notification-state check") }
        let oldNotificationCheck = delegate.beginStateRequest()
        let newPoll = delegate.beginStateRequest()
        guard delegate.acceptStateResponse(initial, serial: newPoll),
              !delegate.acceptStateResponse(state([], false), serial: oldNotificationCheck) else {
            fail("Old notification-state check replaced a newer poll")
        }
        let alerts = desiredResetAlerts(state([base], true), now: Date(timeIntervalSince1970: now))
        guard alerts.map(\.window) == ["Session", "Weekly"], !alerts[0].id.contains("private") else {
            fail("Reset planner omitted a zero-used window or included past/unknown windows or private identity")
        }
        let unused = Account(id: "unused-codex", provider: "codex", email: "", plan: nil,
            active: false, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Weekly", used_percent: 0,
                    resets_at: now + 3600)]), reset_credits: nil)
        guard desiredResetAlerts(state([unused], true), now: Date(timeIntervalSince1970: now)).count == 1 else {
            fail("A zero-used Codex window reset without an opted-in alert")
        }
        guard resetAlertPermission(.authorized, alertSetting: .enabled,
                  alertStyle: .banner, centerSetting: .enabled).canSend,
              resetAlertPermission(.authorized, alertSetting: .disabled,
                  alertStyle: .none, centerSetting: .enabled).message.contains("Notification Center"),
              !resetAlertPermission(.authorized, alertSetting: .disabled,
                  alertStyle: .none, centerSetting: .disabled).canSend,
              !resetAlertPermission(.denied, alertSetting: .disabled,
                  alertStyle: .none, centerSetting: .disabled).canSend,
              resetAlertPermission(.provisional, alertSetting: .enabled,
                  alertStyle: .none, centerSetting: .enabled).message.contains("quietly") else {
            fail("macOS banner, quiet, or blocked delivery was reported incorrectly")
        }
        // The server can poll Codex again just as the reset rolls over. Its
        // new snapshot has a zeroed window and next week's reset timestamp,
        // before the menu's delivery timer has fired for the old timestamp.
        let before = Account(id: "codex-reset", provider: "codex", email: "private@example.com", plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Weekly", used_percent: 25,
                    resets_at: now + 3600)]), reset_credits: nil)
        let planned = desiredResetAlerts(state([before], true), now: Date(timeIntervalSince1970: now))
        guard planned.count == 1 else { fail("Expected one Codex reset alert before rollover") }
        let rolled = Account(id: before.id, provider: "codex", email: "", plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Weekly", used_percent: 0,
                    resets_at: now + 3600 + 7 * 86400)]), reset_credits: nil)
        let afterReset = Date(timeIntervalSince1970: now + 3602)
        let retained = reconciledResetAlerts(planned, state: state([rolled], true), now: afterReset)
        let afterDeliveryCheck = reconciledResetAlerts(retained, state: state([rolled], true),
            now: afterReset.addingTimeInterval(1))
        let rechecked = reconciledResetAlerts(
            reconciledResetAlerts(planned, state: state([before], true), now: afterReset),
            state: state([rolled], true), now: afterReset.addingTimeInterval(1))
        let afterSleep = reconciledResetAlerts(planned, state: state([rolled], true),
            now: afterReset.addingTimeInterval(5 * 60))
        let id = planned[0].id
        let survivesRefresh = retained.contains { $0.id == id }
        let survivesSecondCheck = afterDeliveryCheck.contains { $0.id == id } &&
            rechecked.contains { $0.id == id }
        let survivesSleep = afterSleep.contains { $0.id == id }
        guard survivesRefresh && survivesSecondCheck && survivesSleep else {
            fail("Codex reset alert lost: refresh=\(survivesRefresh), server recheck=\(survivesSecondCheck), delayed timer=\(survivesSleep)")
        }
        guard dueResetAlerts(retained, now: afterReset, delivered: [], sending: []).map(\.id) == [id],
              dueResetAlerts(retained, now: afterReset, delivered: [id], sending: []).isEmpty,
              dueResetAlerts(retained, now: afterReset, delivered: [], sending: [id]).isEmpty else {
            fail("Reset alert was not due exactly once")
        }
        let nextDay = afterReset.addingTimeInterval(12 * 3600)
        guard reconciledResetAlerts(planned, state: state([rolled], true), now: nextDay).contains(where: { $0.id == id }),
              dueResetAlerts(planned, now: nextDay, delivered: [], sending: []).count == 1,
              !reconciledResetAlerts(planned, state: state([rolled], true),
                  now: afterReset.addingTimeInterval(25 * 3600)).contains(where: { $0.id == id }) else {
            fail("Sleeping past a reset lost bounded catch-up or delivered a stale alert")
        }
        guard reconciledResetAlerts(planned, state: state([], true), now: afterReset).isEmpty,
              reconciledResetAlerts(planned, state: state([rolled], false), now: afterReset).isEmpty else {
            fail("Removed accounts or disabled notifications kept a reset alert")
        }
        let missingWindow = Account(id: before.id, provider: "codex", email: "", plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: true, windows: []), reset_credits: nil)
        guard reconciledResetAlerts(planned, state: state([missingWindow], true), now: afterReset).isEmpty else {
            fail("A removed usage window kept its reset alert")
        }
        let unavailable = Account(id: before.id, provider: "codex", email: "", plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: false, windows: nil), reset_credits: nil)
        guard reconciledResetAlerts(planned, state: state([unavailable], true), now: afterReset).map(\.id) == [id] else {
            fail("A temporary usage outage erased a scheduled reset")
        }
        let shortlyBefore = Date(timeIntervalSince1970: now + 3600 - 90)
        guard reconciledResetAlerts(planned, state: state([rolled], true), now: shortlyBefore).contains(where: { $0.id == id }),
              dueResetAlerts(planned, now: shortlyBefore, delivered: [], sending: []).isEmpty else {
            fail("A near-reset upstream rollover was not retained until its reported time")
        }
        let farBefore = Date(timeIntervalSince1970: now + 3600 - 10 * 60)
        guard !reconciledResetAlerts(planned, state: state([rolled], true), now: farBefore).contains(where: { $0.id == id }) else {
            fail("An old reset stayed scheduled after an early upstream change")
        }
        let corrected = Account(id: before.id, provider: "codex", email: "", plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Weekly", used_percent: 25,
                    resets_at: now + 4000)]), reset_credits: nil)
        let revised = reconciledResetAlerts(planned, state: state([corrected], true),
            now: Date(timeIntervalSince1970: now + 10))
        guard revised.count == 1, revised[0].id != id else {
            fail("An upstream reset correction kept the obsolete alert")
        }
        let other = Account(id: "second-codex", provider: "codex", email: "", plan: nil,
            active: false, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Weekly", used_percent: 0,
                    resets_at: now + 3600)]), reset_credits: nil)
        let twoPlans = desiredResetAlerts(state([before, other], true), now: Date(timeIntervalSince1970: now))
        let otherRolled = Account(id: other.id, provider: "codex", email: "", plan: nil,
            active: false, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Weekly", used_percent: 0,
                    resets_at: now + 3600 + 7 * 86400)]), reset_credits: nil)
        let twoDue = dueResetAlerts(reconciledResetAlerts(twoPlans,
            state: state([rolled, otherRolled], true), now: afterReset),
            now: afterReset, delivered: [], sending: [])
        guard Set(twoDue.map(\.accountID)) == Set([before.id, other.id]),
              Set(twoDue.map(\.id)).count == 2 else {
            fail("Two Codex accounts resetting together did not produce separate alerts")
        }
        var pending = twoDue
        var deliveredIDs = Set<String>()
        recordDeliveredResetAlert(twoDue[0].id, alerts: &pending, delivered: &deliveredIDs)
        guard pending.map(\.id) == [twoDue[1].id],
              dueResetAlerts(pending, now: afterReset, delivered: deliveredIDs, sending: []).map(\.id) == [twoDue[1].id] else {
            fail("Delivering one account's alert erased or repeated another account's alert")
        }
        let ledgerURL = FileManager.default.temporaryDirectory
            .appendingPathComponent("switcher-reset-test-" + UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: ledgerURL) }
        let ledgerFile = ledgerURL.appendingPathComponent("reset-alerts.json")
        do {
            try FileManager.default.createDirectory(at: ledgerURL, withIntermediateDirectories: true)
            try saveResetAlertLedger(ResetAlertLedger(alerts: planned, deliveredIDs: []), at: ledgerFile)
            let restored = try loadResetAlertLedger(at: ledgerFile)
            guard reconciledResetAlerts(restored.alerts, state: state([rolled], true), now: afterReset).contains(where: { $0.id == id }),
                  restored.deliveredIDs.isEmpty else { fail("A menu restart lost the pending reset") }
            let raw = try Data(contentsOf: ledgerFile)
            let permissions = try FileManager.default.attributesOfItem(atPath: ledgerFile.path)[.posixPermissions] as? NSNumber
            guard !String(decoding: raw, as: UTF8.self).contains("private@example.com"),
                  permissions?.intValue == 0o600 else { fail("Reset ledger revealed identity or was not private") }
            try saveResetAlertLedger(ResetAlertLedger(alerts: retained, deliveredIDs: [id]), at: ledgerFile)
            let delivered = try loadResetAlertLedger(at: ledgerFile)
            guard dueResetAlerts(delivered.alerts, now: afterReset, delivered: Set(delivered.deliveredIDs),
                sending: []).isEmpty else { fail("A menu restart repeated a delivered notification") }
        } catch {
            fail("Reset ledger persistence failed: \(error)")
        }
        let shifted = Account(id: "a", provider: "codex", email: base.email, plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Session", used_percent: 25, resets_at: now + 3700)]), reset_credits: nil)
        let newAlerts = desiredResetAlerts(state([shifted], true), now: Date(timeIntervalSince1970: now))
        guard newAlerts.count == 1, newAlerts[0].id != alerts[0].id,
              desiredResetAlerts(state([], true)).isEmpty,
              desiredResetAlerts(state([base], false)).isEmpty else {
            fail("Reset alert shift, deletion, or disable planning failed")
        }
        let many = (0..<40).map { i in
            Account(id: "account-\(i)", provider: "codex", email: "", plan: nil,
                active: false, exhausted_until: nil, usage: Usage(available: true,
                    windows: [UsageWindow(label: "Session", used_percent: 1, resets_at: now + Double(40-i)*60)]), reset_credits: nil)
        }
        let capped = desiredResetAlerts(state(many, true), now: Date(timeIntervalSince1970: now))
        guard capped.count == 32, capped[0].id.contains("account-39") else {
            fail("Reset planner did not choose the earliest 32 windows")
        }
        guard logoUsesTemplate("copilot"), logoUsesTemplate("grok"), !logoUsesTemplate("gemini"),
              let copilot = NSImage(contentsOfFile: "build/macos/logos/copilot.svg") else {
            fail("Copilot logo must load as a tintable template")
        }
        copilot.isTemplate = logoUsesTemplate("copilot")
        func logoBrightness(_ tint: NSColor) -> CGFloat {
            let view = NSImageView(frame: NSRect(x: 0, y: 0, width: 22, height: 22))
            view.image = copilot
            view.contentTintColor = tint
            guard let rep = renderBitmap(view) else { fail("Copilot mark did not render") }
            var total: CGFloat = 0
            var pixels = 0
            for y in 0..<rep.pixelsHigh {
                for x in 0..<rep.pixelsWide {
                    guard let color = rep.colorAt(x: x, y: y)?.usingColorSpace(.deviceRGB),
                          color.alphaComponent > 0.5 else { continue }
                    total += (color.redComponent + color.greenComponent + color.blueComponent) / 3
                    pixels += 1
                }
            }
            guard pixels > 0 else { fail("Copilot mark has no visible pixels") }
            return total / CGFloat(pixels)
        }
        guard logoBrightness(Palette.dark.ink) > logoBrightness(Palette.light.ink) + 0.35 else {
            fail("Copilot mark did not follow light and dark menu colours")
        }
        print("Menu bar layout, reset alerts, and Copilot theme tint: OK")
    }
}
