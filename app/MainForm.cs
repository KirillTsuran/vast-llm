using System.Diagnostics;

namespace VastLLM;

static class Program
{
    [STAThread]
    static void Main()
    {
        using var mutex = new Mutex(true, "VastLLM-single-instance", out bool first);
        if (!first) // already running: ask the first copy to show its window
        {
            try { EventWaitHandle.OpenExisting("VastLLM-show").Set(); } catch { }
            return;
        }
        ApplicationConfiguration.Initialize();
        Application.Run(new MainForm());
    }
}

public class MainForm : Form
{
    readonly Core core = new();
    readonly Label phase = new() { AutoSize = true, Font = new Font("Segoe UI", 14, FontStyle.Bold) };
    readonly Label msg = new() { AutoSize = true, ForeColor = SystemColors.GrayText };
    readonly Label info = new() { AutoSize = true };
    readonly TextBox url = new() { ReadOnly = true, Width = 260 };
    readonly Button up = new() { Text = "Up", Width = 110, Height = 34 }, down = new() { Text = "Down", Width = 110, Height = 34 };
    readonly Button copy = new() { Text = "Копировать", AutoSize = true }, folder = new() { Text = "Папка и config.json", AutoSize = true };
    readonly Button grafana = new() { Text = "Grafana", Width = 110, Height = 34 };
    readonly TextBox log = new() { Multiline = true, ReadOnly = true, ScrollBars = ScrollBars.Vertical, Dock = DockStyle.Fill, Font = new Font("Consolas", 9) };
    readonly NotifyIcon tray = new() { Icon = SystemIcons.Application, Text = "VastLLM", Visible = true };
    readonly System.Windows.Forms.Timer ui = new() { Interval = 1000 }, tick = new() { Interval = 30000 };
    bool exiting;

    public MainForm()
    {
        Text = "VastLLM — GPU для ZCode"; Width = 640; Height = 480; StartPosition = FormStartPosition.CenterScreen;
        Icon = SystemIcons.Application;
        var top = new FlowLayoutPanel { Dock = DockStyle.Top, AutoSize = true, FlowDirection = FlowDirection.TopDown, Padding = new Padding(12), WrapContents = false };
        var urlRow = new FlowLayoutPanel { AutoSize = true, WrapContents = false };
        urlRow.Controls.AddRange(new Control[] { new Label { Text = "ZCode URL:", AutoSize = true, Padding = new Padding(0, 6, 0, 0) }, url, copy });
        var btnRow = new FlowLayoutPanel { AutoSize = true, WrapContents = false };
        btnRow.Controls.AddRange(new Control[] { up, down, grafana, folder });
        top.Controls.AddRange(new Control[] { phase, msg, info, urlRow, btnRow });
        Controls.Add(log); Controls.Add(top);

        core.Logged += l => BeginInvokeSafe(() => { log.AppendText(l + Environment.NewLine); });
        core.Changed += () => BeginInvokeSafe(Refresh2);
        up.Click += async (_, _) => await core.Up();
        down.Click += async (_, _) => await core.Down("вручную");
        copy.Click += (_, _) => Clipboard.SetText(url.Text);
        folder.Click += (_, _) => Process.Start("explorer.exe", core.Dir);
        grafana.Click += (_, _) => Process.Start(new ProcessStartInfo($"http://127.0.0.1:{core.Cfg.GrafanaPort}/d/llm") { UseShellExecute = true });
        var show = new EventWaitHandle(false, EventResetMode.AutoReset, "VastLLM-show");
        new Thread(() => { while (show.WaitOne()) BeginInvokeSafe(() => { Show(); WindowState = FormWindowState.Normal; Activate(); }); }) { IsBackground = true }.Start();
        ui.Tick += (_, _) => Refresh2();
        tick.Tick += async (_, _) => await core.Tick();
        tray.DoubleClick += (_, _) => { Show(); WindowState = FormWindowState.Normal; Activate(); };
        tray.ContextMenuStrip = new ContextMenuStrip();
        tray.ContextMenuStrip.Items.Add("Открыть", null, (_, _) => { Show(); WindowState = FormWindowState.Normal; });
        tray.ContextMenuStrip.Items.Add("Up", null, async (_, _) => await core.Up());
        tray.ContextMenuStrip.Items.Add("Down", null, async (_, _) => await core.Down("вручную"));
        tray.ContextMenuStrip.Items.Add("Выход", null, (_, _) => Close());
        Resize += (_, _) => { if (WindowState == FormWindowState.Minimized) { Hide(); tray.ShowBalloonTip(2000, "VastLLM", "Работает в трее", ToolTipIcon.Info); } };
        FormClosing += OnClosing;
        Load += async (_, _) => { ui.Start(); tick.Start(); await core.Startup(); Refresh2(); };
    }

    void BeginInvokeSafe(Action a) { if (IsHandleCreated && !IsDisposed) BeginInvoke(a); }

    static readonly Dictionary<string, string> Names = new()
    {
        ["off"] = "Выключено", ["renting"] = "Аренда", ["booting"] = "Запуск", ["downloading"] = "Скачивание модели",
        ["loading"] = "Загрузка модели", ["ready"] = "Работает", ["stopping"] = "Удаление", ["error"] = "Ошибка",
    };

    void Refresh2()
    {
        phase.Text = Names.GetValueOrDefault(core.Phase, core.Phase);
        phase.ForeColor = core.Phase switch { "ready" => Color.SeaGreen, "error" => Color.Firebrick, "off" => SystemColors.ControlText, _ => Color.DarkOrange };
        msg.Text = core.Message;
        url.Text = $"http://127.0.0.1:{core.Cfg.LocalPort}/v1   модель: {core.Cfg.Model}";
        var s = core.St; var r = core.Cfg.UsdRub;
        var all = core.AllRented();
        if (all.Count > 0)
        {
            var lines = all.Select(x =>
            {
                var h = (DateTime.UtcNow - x.Created).TotalHours;
                return $"{(x.Main ? "●" : "○")} #{x.Id} · {x.Gpu} · {x.Geo} · {x.Dph * r:F1} ₽/ч · {(int)(h * 60)} мин · ≈ {h * x.Dph * r:F0} ₽{(x.Main ? "" : " (запуск)")}";
            });
            info.Text = $"Арендовано в Vast ({all.Count}), всего {all.Sum(x => x.Dph) * r:F1} ₽/ч:\n" + string.Join("\n", lines) +
                        (core.Phase == "ready" ? $"\nТуннель {(core.Tunnel ? "есть" : "нет")} · без запросов {core.IdleMin} мин · удаляется только кнопкой Down" : "");
        }
        else info.Text = $"Ничего не арендовано · GPU: {core.Cfg.Gpu} до {core.Cfg.MaxDph * r:F0} ₽/ч · курс {r:F2} ₽/$";
        bool busy = core.Phase is "renting" or "booting" or "downloading" or "loading" or "stopping";
        up.Enabled = !busy && core.Phase != "ready";
        down.Enabled = all.Count > 0 && core.Phase != "stopping";
        grafana.Enabled = core.Tunnel;
        tray.Text = "VastLLM: " + phase.Text;
    }

    async void OnClosing(object sender, FormClosingEventArgs e)
    {
        if (exiting || core.AllRented().Count == 0) { tray.Visible = false; return; }
        e.Cancel = true;
        var r = MessageBox.Show($"Арендовано машин: {core.AllRented().Count} (≈{core.AllRented().Sum(x => x.Dph) * core.Cfg.UsdRub:F0} ₽/ч).\n\nДа — удалить машины и выйти\nНет — выйти, машины останутся и будут оплачиваться; при следующем запуске программа их подхватит\nОтмена — не выходить",
            "VastLLM", MessageBoxButtons.YesNoCancel, MessageBoxIcon.Warning);
        if (r == DialogResult.Cancel) return;
        if (r == DialogResult.Yes) { Show(); await core.Down("выход из программы"); }
        exiting = true; tray.Visible = false; Close();
    }
}
