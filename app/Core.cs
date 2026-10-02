// Vast.ai logic: config/state next to the exe, offers, renting (2-host race), SSH tunnel, auto-down, watchdog heartbeat.
using System.Globalization;
using System.Net.Http.Headers;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.Json.Serialization;
using System.Text.RegularExpressions;
using Renci.SshNet;

namespace VastLLM;

public class Config
{
    [JsonPropertyName("vast_api_key")] public string VastKey { get; set; } = "";
    [JsonPropertyName("gpu")] public string Gpu { get; set; } = "RTX 3090";
    // GPU models offered in the window; Up rents the cheapest offers of the chosen one, no price limit
    public static readonly string[] Gpus = { "RTX 3090", "RTX 4090", "RTX 5090" };
    // the race rents the 2 cheapest; if the pricier one comes up first, the cheapest gets this many seconds to catch up
    [JsonPropertyName("cheapest_grace_seconds")] public int Grace { get; set; } = 20;
    [JsonPropertyName("datacenter_only")] public bool Datacenter { get; set; } = false;
    [JsonPropertyName("race_two_hosts")] public bool Race { get; set; } = true;
    [JsonPropertyName("local_port")] public int LocalPort { get; set; } = 8080;
    [JsonPropertyName("grafana_port")] public int GrafanaPort { get; set; } = 3000;
    [JsonPropertyName("image")] public string Image { get; set; } = "ghcr.io/kirilltsuran/vast-llm:latest";
    [JsonPropertyName("model")] public string Model { get; set; } = "qwen3.8-27b-uncensored";
    [JsonPropertyName("usd_rub")] public double UsdRub { get; set; } = 83.56;
    // start with Windows (minimized to the tray) and reconnect to the rented machine by itself
    [JsonPropertyName("autostart")] public bool Autostart { get; set; } = true;
    // logs\vast-llm-<date>.log next to the exe; files older than this are deleted
    [JsonPropertyName("log_retention_days")] public int LogDays { get; set; } = 30;
    // extra "debug:" lines in the log file (API calls, SSH attempts, tunnel statistics); the window shows only the main events
    [JsonPropertyName("debug_log")] public bool Debug { get; set; } = true;
}

public class State
{
    [JsonPropertyName("instance_id")] public long Id { get; set; }
    [JsonPropertyName("gpu")] public string Gpu { get; set; } = "";
    [JsonPropertyName("geo")] public string Geo { get; set; } = "";
    [JsonPropertyName("usd_per_hour")] public double Dph { get; set; }
    [JsonPropertyName("created")] public DateTime Created { get; set; }
    [JsonPropertyName("host_key")] public string HostKey { get; set; } = "";
    [JsonPropertyName("bad_machines")] public List<long> BadMachines { get; set; } = new();
    // machines rented by an Up that has not picked its winner yet (persisted right after renting)
    [JsonPropertyName("pending")] public List<Rented> Pending { get; set; } = new();
}

public class Rented
{
    [JsonPropertyName("id")] public long Id { get; set; }
    [JsonPropertyName("machine")] public long Machine { get; set; }
    [JsonPropertyName("gpu")] public string Gpu { get; set; } = "";
    [JsonPropertyName("geo")] public string Geo { get; set; } = "";
    [JsonPropertyName("usd_per_hour")] public double Dph { get; set; }
    [JsonPropertyName("created")] public DateTime Created { get; set; }
    [JsonIgnore] public bool Main { get; set; }
}

public record Offer(long Id, long Machine, double Dph, double InetCost, double Inet, string Geo, string Gpu);

public class Core
{
    public const string Label = "vast-llm";
    static readonly JsonSerializerOptions JsonOpt = new() { WriteIndented = true };
    public readonly string Dir = AppContext.BaseDirectory;
    string P(string f) => Path.Combine(Dir, f);

    public Config Cfg = new();
    public State St = new();
    public string Phase = "off", Message = "";
    public event Action Changed;
    public event Action<string> Logged;

    SshClient client;
    ForwardedPortLocal fwd, fwdGrafana;
    PrivateKeyFile key;
    string pub;
    readonly SemaphoreSlim busy = new(1, 1);
    CancellationTokenSource upCts;
    DateTime lastUse = DateTime.UtcNow, lastStats = DateTime.UtcNow;
    int tunnelRequests, ticking;
    bool lowCreditWarned;
    public double? Credit;  // Vast balance in dollars (credit + balance); Vast stops every machine when it runs out
    DateTime lastCredit = DateTime.MinValue;
    readonly HttpClient http = new() { Timeout = TimeSpan.FromSeconds(40) };
    readonly object stateLock = new();

    public bool Tunnel => client?.IsConnected == true && fwd?.IsStarted == true;
    public int IdleMin => (int)(DateTime.UtcNow - lastUse).TotalMinutes;

    // ---------- files ----------
    public string LoadConfig()
    {
        var path = P("config.json");
        if (!File.Exists(path)) { SaveConfig(); return "создан config.json рядом с программой: впишите vast_api_key"; }
        try { Cfg = JsonSerializer.Deserialize<Config>(File.ReadAllText(path)) ?? new Config(); SaveConfig(); }
        catch (Exception e) { return "config.json не читается: " + e.Message; }
        return Cfg.VastKey.Trim() == "" ? "впишите vast_api_key в config.json и нажмите Up" : null;
    }
    public void SaveConfig() => File.WriteAllText(P("config.json"), JsonSerializer.Serialize(Cfg, JsonOpt));
    void SaveState() // atomic: a crash or power loss never leaves a half-written state.json
    {
        lock (stateLock)
        {
            var tmp = P("state.json.tmp");
            File.WriteAllText(tmp, JsonSerializer.Serialize(St, JsonOpt));
            File.Move(tmp, P("state.json"), true);
        }
    }
    public void LoadState()
    {
        try { if (File.Exists(P("state.json"))) St = JsonSerializer.Deserialize<State>(File.ReadAllText(P("state.json"))) ?? new State(); } catch { St = new State(); }
    }

    public void Log(string s)
    {
        Append(s);
        Logged?.Invoke(DateTime.Now.ToString("HH:mm:ss ") + s);
    }
    public void Debug(string s) { if (Cfg.Debug) Append("debug: " + s); }

    // the log file only; usable before the window exists and from crash handlers (one file per day)
    public static readonly string LogDir = Path.Combine(AppContext.BaseDirectory, "logs");
    static readonly object logLock = new();
    public static void Append(string s)
    {
        lock (logLock)
            try
            {
                Directory.CreateDirectory(LogDir);
                File.AppendAllText(Path.Combine(LogDir, $"vast-llm-{DateTime.Now:yyyy-MM-dd}.log"), $"{DateTime.Now:yyyy-MM-dd HH:mm:ss.fff} {s}{Environment.NewLine}");
            }
            catch { }
    }
    // deletes day logs older than log_retention_days; the single log of versions before 1.3 moves into logs\
    void CleanLogs()
    {
        try
        {
            var old = P("vast-llm.log");
            if (File.Exists(old)) File.Move(old, Path.Combine(LogDir, "vast-llm-before-1.3.log"), true);
            foreach (var f in Directory.GetFiles(LogDir, "vast-llm-*.log"))
                if (File.GetLastWriteTime(f) < DateTime.Now.AddDays(-Math.Max(1, Cfg.LogDays))) { File.Delete(f); Debug("удалён старый журнал " + Path.GetFileName(f)); }
        }
        catch (Exception e) { Debug("очистка журналов: " + e.Message); }
    }

    // HKCU Run entry follows config.json "autostart"; --tray starts the window hidden
    public void SyncAutostart()
    {
        const string run = @"Software\Microsoft\Windows\CurrentVersion\Run";
        try
        {
            using var k = Microsoft.Win32.Registry.CurrentUser.OpenSubKey(run, true);
            var want = $"\"{Environment.ProcessPath}\" --tray";
            var have = k?.GetValue("VastLLM") as string;
            if (Cfg.Autostart && have != want) { k?.SetValue("VastLLM", want); Log("автозапуск при входе в Windows включён"); }
            else if (!Cfg.Autostart && have != null) { k?.DeleteValue("VastLLM", false); Log("автозапуск выключен"); }
        }
        catch (Exception e) { Log("автозапуск: " + e.Message); }
    }
    void Set(string phase, string msg)
    {
        bool ch = Phase != phase || Message != msg;
        Phase = phase; Message = msg;
        if (ch) { Log($"[{phase}] {msg}"); Changed?.Invoke(); }
    }

    // ECDSA P-256 key for SSH (kept next to the exe). Short public key: it travels to the container in an env variable.
    public void LoadKey()
    {
        var kp = P("ssh_key.pem");
        if (!File.Exists(kp)) { using var ec = ECDsa.Create(ECCurve.NamedCurves.nistP256); File.WriteAllText(kp, ec.ExportECPrivateKeyPem()); }
        using (var ec = ECDsa.Create())
        {
            ec.ImportFromPem(File.ReadAllText(kp));
            var q = ec.ExportParameters(false).Q;
            var ms = new MemoryStream();
            void Str(byte[] b) { var l = BitConverter.GetBytes(b.Length); Array.Reverse(l); ms.Write(l); ms.Write(b); }
            Str(Encoding.ASCII.GetBytes("ecdsa-sha2-nistp256")); Str(Encoding.ASCII.GetBytes("nistp256"));
            Str(new byte[] { 4 }.Concat(q.X).Concat(q.Y).ToArray());
            pub = "ecdsa-sha2-nistp256 " + Convert.ToBase64String(ms.ToArray()) + " " + Label;
        }
        key = new PrivateKeyFile(kp);
    }

    // ---------- CBR rate ----------
    public async Task RateAsync()
    {
        try
        {
            var b = await http.GetByteArrayAsync("https://www.cbr.ru/scripts/XML_daily.asp");
            var m = Regex.Match(Encoding.Latin1.GetString(b), @"<CharCode>USD</CharCode>.*?<Value>([0-9,]+)</Value>", RegexOptions.Singleline);
            if (m.Success && double.TryParse(m.Groups[1].Value.Replace(',', '.'), NumberStyles.Float, CultureInfo.InvariantCulture, out var v) && v > 0)
            { Cfg.UsdRub = v; SaveConfig(); Log($"курс ЦБ: {v:F2} ₽/$"); Changed?.Invoke(); }
        }
        catch { Log($"курс ЦБ недоступен, использую {Cfg.UsdRub:F2}"); }
    }

    // ---------- Vast API ----------
    async Task<JsonNode> Api(HttpMethod m, string path, object body = null)
    {
        var url = (path.StartsWith("/v1/") ? "https://console.vast.ai/api" : "https://console.vast.ai/api/v0") + path;
        Exception last = null;
        for (int i = 0; i < 4; i++)
        {
            try
            {
                var req = new HttpRequestMessage(m, url);
                req.Headers.Authorization = new AuthenticationHeaderValue("Bearer", Cfg.VastKey.Trim());
                if (body != null) req.Content = new StringContent(JsonSerializer.Serialize(body), Encoding.UTF8, "application/json");
                var sw = System.Diagnostics.Stopwatch.StartNew();
                var r = await http.SendAsync(req);
                var txt = await r.Content.ReadAsStringAsync();
                Debug($"vast {m.Method} {path} -> {(int)r.StatusCode} за {sw.ElapsedMilliseconds} мс{(i > 0 ? $", попытка {i + 1}" : "")}");
                if (r.IsSuccessStatusCode) return string.IsNullOrWhiteSpace(txt) ? null : JsonNode.Parse(txt);
                last = new Exception($"HTTP {(int)r.StatusCode}: {txt[..Math.Min(300, txt.Length)]}");
                if ((int)r.StatusCode < 500 && (int)r.StatusCode != 429) throw last;
            }
            catch (Exception e) when (e != last) { last = e; Debug($"vast {m.Method} {path} ошибка: {e.Message}"); }
            await Task.Delay(3000 * (i + 1));
        }
        throw last;
    }

    // "/v1/instances/" with the trailing slash: Vast answers 301 without it and .NET drops the auth header on redirect
    async Task<List<JsonNode>> Instances() => (await Api(HttpMethod.Get, "/v1/instances/"))?["instances"]?.AsArray().ToList() ?? new();
    async Task<JsonNode> Instance(long id) => (await Instances()).FirstOrDefault(i => (long)i["id"] == id);

    async Task<List<Offer>> Offers()
    {
        var q = new Dictionary<string, object>
        {
            ["type"] = "on-demand", ["limit"] = 200, ["num_gpus"] = new { eq = 1 }, ["gpu_name"] = new { eq = Cfg.Gpu },
            ["rentable"] = new { eq = true }, ["rented"] = new { eq = false }, ["verified"] = new { eq = true },
            ["cuda_max_good"] = new { gte = 12.8 }, ["disk_space"] = new { gte = 60 }, ["cpu_ram"] = new { gte = 32000 },
            ["inet_down"] = new { gte = 500 }, ["direct_port_count"] = new { gte = 1 }, ["reliability"] = new { gte = 0.98 },
            ["order"] = new[] { new[] { "dph_total", "asc" } }, ["allocated_storage"] = 60,
        };
        if (Cfg.Datacenter) q["datacenter"] = new { eq = true };
        var r = await Api(HttpMethod.Post, "/bundles/", q);
        var list = new List<Offer>();
        foreach (var o in r?["offers"]?.AsArray() ?? new JsonArray())
        {
            var off = new Offer((long)o["id"], (long)o["machine_id"], (double)o["dph_total"], (double?)o["inet_down_cost"] ?? 0,
                                (double?)o["inet_down"] ?? 0, (string)o["geolocation"] ?? "", (string)o["gpu_name"] ?? "");
            if (off.InetCost <= 0.01 && !St.BadMachines.Contains(off.Machine) && !off.Geo.Contains("CN")) list.Add(off);
            else Debug($"предложение {off.Id} ({off.Geo}, ${off.Dph:F3}/ч) отброшено: {(off.InetCost > 0.01 ? "платный трафик" : St.BadMachines.Contains(off.Machine) ? "машина в чёрном списке" : "Китай")}");
        }
        list = list.OrderBy(o => o.Dph).ThenByDescending(o => o.Inet).ToList();
        Debug($"предложений {Cfg.Gpu}: {list.Count}, самые дешёвые: {string.Join(", ", list.Take(3).Select(o => $"${o.Dph:F3} {o.Geo}"))}");
        return list;
    }

    async Task Destroy(long id)
    {
        try { await Api(HttpMethod.Delete, $"/instances/{id}/"); } catch (Exception e) { Log($"delete {id}: {e.Message}"); }
        for (int i = 0; i < 40; i++)
        {
            try { if (await Instance(id) == null) { Log($"машина {id} удалена (подтверждено API)"); return; } } catch { }
            await Task.Delay(5000);
        }
        throw new Exception($"удаление {id} не подтверждено, проверьте console.vast.ai");
    }

    async Task<long> Rent(Offer o)
    {
        var pk = Convert.ToBase64String(Encoding.UTF8.GetBytes(pub));
        var body = new Dictionary<string, object>
        {
            ["image"] = Cfg.Image, ["label"] = Label, ["disk"] = 60, ["runtype"] = "args", ["args"] = new[] { "bash", "/opt/llm/entry.sh" },
            ["target_state"] = "running", ["cancel_unavail"] = true, ["env"] = $"-e PUBKEY_B64={pk} -e WATCHDOG_MIN=525600 -p 22:22",
        };
        var r = await Api(HttpMethod.Put, $"/asks/{o.Id}/", body);
        var id = (long?)r?["new_contract"] ?? 0;
        if (r?["success"]?.GetValue<bool>() != true || id == 0) throw new Exception("offer not accepted");
        try { await Api(HttpMethod.Post, $"/instances/{id}/ssh", new { ssh_key = pub }); } catch (Exception e) { Log($"привязка ключа к {id}: {e.Message}"); }
        return id;
    }

    // Vast writes the account's SSH keys into every container, so the program's key must be among them
    async Task EnsureAccountKey()
    {
        var body = pub.Split(' ')[1];
        var keys = await Api(HttpMethod.Get, "/ssh/");
        foreach (var k in keys?.AsArray() ?? new JsonArray())
            if (((string)k?["public_key"] ?? "").Contains(body)) return;
        await Api(HttpMethod.Post, "/ssh/", new { ssh_key = pub });
        Log("SSH-ключ программы добавлен в аккаунт Vast");
    }

    // waits for the container and opens SSH to its own sshd (port 22 mapped to a random external port)
    async Task<(SshClient, string)> Dial(long id, string pinned, TimeSpan boot, CancellationToken ct)
    {
        var deadline = DateTime.UtcNow + boot; DateTime? running = null; Exception last = new("машина не запустилась вовремя");
        string lastApiErr = null;
        while (DateTime.UtcNow < deadline)
        {
            ct.ThrowIfCancellationRequested();
            JsonNode inst = null;
            try { inst = await Instance(id); if (inst == null) throw new Exception("машина исчезла"); }
            catch (Exception e) when (e.Message == "машина исчезла") { throw; }
            catch (Exception e) { if (e.Message != lastApiErr) { lastApiErr = e.Message; Log("Vast API: " + e.Message); } }
            if (inst != null && (string)inst["actual_status"] == "running" && !string.IsNullOrEmpty((string)inst["public_ipaddr"]))
            {
                running ??= DateTime.UtcNow;
                var port = int.TryParse((string)inst["ports"]?["22/tcp"]?[0]?["HostPort"], out var pp) ? pp : 0;
                if (port > 0)
                {
                    var hk = pinned;
                    var ci = new ConnectionInfo((string)inst["public_ipaddr"], port, "root", new PrivateKeyAuthenticationMethod("root", key)) { Timeout = TimeSpan.FromSeconds(15) };
                    var c = new SshClient(ci) { KeepAliveInterval = TimeSpan.FromSeconds(20) };
                    c.HostKeyReceived += (_, e) =>
                    {
                        if (string.IsNullOrEmpty(hk)) { hk = e.FingerPrintSHA256; e.CanTrust = true; }
                        else e.CanTrust = hk == e.FingerPrintSHA256;
                    };
                    try { await Task.Run(() => c.Connect(), ct); Debug($"ssh {inst["public_ipaddr"]}:{port} машины {id}: подключено"); return (c, hk); }
                    catch (Exception e) { last = e; c.Dispose(); Debug($"ssh {inst["public_ipaddr"]}:{port} машины {id}: {e.Message}"); }
                }
                if (DateTime.UtcNow - running > TimeSpan.FromMinutes(3)) throw new Exception("SSH недоступен 3 мин после старта: " + last.Message);
            }
            await Task.Delay(6000, ct);
        }
        throw last;
    }

    void Attach(SshClient c)
    {
        CloseClient();
        client = c;
        // logged at once (the 30 s tick reconnects); a dead session is the usual reason ZCode sees ECONNREFUSED
        c.ErrorOccurred += (_, e) => Log("туннель: SSH-сессия оборвалась — " + e.Exception.Message);
        fwd = new ForwardedPortLocal("127.0.0.1", (uint)Cfg.LocalPort, "127.0.0.1", 8080);
        fwd.RequestReceived += (_, _) => { lastUse = DateTime.UtcNow; Interlocked.Increment(ref tunnelRequests); };
        client.AddForwardedPort(fwd);
        fwd.Start();
        try
        {
            fwdGrafana = new ForwardedPortLocal("127.0.0.1", (uint)Cfg.GrafanaPort, "127.0.0.1", 3000);
            client.AddForwardedPort(fwdGrafana);
            fwdGrafana.Start();
        }
        catch (Exception e) { Log($"Grafana: порт {Cfg.GrafanaPort} занят ({e.Message})"); }
        Exec("touch /opt/llm/heartbeat");
        // the card's power limit matters: the same GPU capped by its host runs up to 2x slower
        var gpu = Exec("nvidia-smi --query-gpu=name,power.limit,power.default_limit,memory.total,driver_version --format=csv,noheader")?.Trim();
        Log("видеокарта машины: " + (string.IsNullOrEmpty(gpu) ? "не удалось узнать" : gpu));
    }

    void CloseClient()
    {
        try { fwd?.Stop(); } catch { }
        try { fwdGrafana?.Stop(); } catch { }
        try { client?.Dispose(); } catch { }
        fwd = null; fwdGrafana = null; client = null;
    }

    string Exec(string cmd)
    {
        try { return client?.IsConnected == true ? client.RunCommand(cmd).Result : null; }
        catch (Exception e) { Debug($"команда на машине не выполнилась ({cmd.Split(' ')[0]}): {e.Message}"); return null; }
    }

    // ---------- Up / Down ----------
    public async Task Up()
    {
        var err = LoadConfig();
        if (err != null && Cfg.VastKey.Trim() == "") { Set("error", err); return; }
        await CheckCredit();
        if (Credit is < 0.5) { Set("error", $"на балансе Vast ${Credit:F2} — пополните на console.vast.ai, иначе Vast сразу остановит машину"); return; }
        if (!await busy.WaitAsync(0)) return;
        upCts = new CancellationTokenSource(); var ct = upCts.Token;
        var start = DateTime.UtcNow;
        try
        {
            await EnsureAccountKey();
            if (St.Id != 0 && !Tunnel)
            {
                Set("booting", $"подключаюсь к машине {St.Id}");
                try { var (c, hk) = await Dial(St.Id, St.HostKey, TimeSpan.FromMinutes(5), ct); St.HostKey = hk; SaveState(); Attach(c); }
                catch (OperationCanceledException) { throw; }
                catch (Exception e) { Set("error", $"машина {St.Id} не отвечает по SSH ({e.Message}). Она не удалена: Up — повторить, Down — удалить"); return; }
            }
            for (int round = 1; St.Id == 0 && round <= 3; round++)
            {
                // candidates: machines already rented by an interrupted Up (state.json), otherwise new offers
                var cands = St.Pending.ToList();
                if (cands.Count == 0)
                {
                    var offs = await Offers();
                    if (offs.Count == 0) { Set("error", $"в Vast сейчас нет свободных {Cfg.Gpu} — выберите другую видеокарту или повторите Up"); return; }
                    Set("renting", Cfg.Race ? $"арендую 2 самые дешёвые {Cfg.Gpu}, оставлю первую поднявшуюся (дешёвой даю {Cfg.Grace} с форы)" : $"арендую самую дешёвую {Cfg.Gpu}");
                    foreach (var o in offs.Take(Cfg.Race ? 2 : 1))
                    {
                        try
                        {
                            var id = await Rent(o);
                            var r = new Rented { Id = id, Machine = o.Machine, Gpu = o.Gpu, Geo = o.Geo, Dph = o.Dph, Created = DateTime.UtcNow };
                            St.Pending.Add(r); SaveState(); cands.Add(r);
                            Log($"арендована {id}: {o.Gpu} {o.Geo} {o.Dph * Cfg.UsdRub:F1} ₽/ч, сеть {(int)o.Inet} Мбит/с");
                        }
                        catch (Exception e) { Log($"аренда не удалась ({o.Geo}): {e.Message}"); St.BadMachines.Add(o.Machine); }
                    }
                    if (cands.Count == 0) continue;
                }
                else Log($"продолжаю прерванный запуск: машины {string.Join(", ", cands.Select(x => x.Id))}");
                Set("booting", "машина качает образ и запускается (~5 мин)");
                using var race = CancellationTokenSource.CreateLinkedTokenSource(ct);
                var tasks = cands.Select(r => Task.Run(async () =>
                {
                    try { var (c, hk) = await Dial(r.Id, "", TimeSpan.FromMinutes(10), race.Token); return (r, c, hk, (Exception)null); }
                    catch (Exception e) { return (r, (SshClient)null, "", e); }
                })).ToList();
                var byId = cands.Zip(tasks).ToDictionary(p => p.First.Id, p => p.Second);
                bool won = false;
                while (tasks.Count > 0)
                {
                    var t = await Task.WhenAny(tasks); tasks.Remove(t);
                    var (r, c, hk, e) = t.Result;
                    if (e != null || won)
                    {
                        if (e != null && !won && e is not OperationCanceledException) { Log($"машина {r.Id} не подошла: {e.Message}"); St.BadMachines.Add(r.Machine); }
                        c?.Dispose();
                        St.Pending.RemoveAll(x => x.Id == r.Id); SaveState();
                        _ = DestroyPending(r.Id);
                        continue;
                    }
                    // a pricier machine came up first: give the cheapest one a few seconds to catch up
                    var cheap = cands.MinBy(x => x.Dph);
                    if (cheap.Id != r.Id && byId.TryGetValue(cheap.Id, out var cheapTask) && tasks.Contains(cheapTask))
                    {
                        Log($"первой поднялась {r.Id} ({r.Dph * Cfg.UsdRub:F1} ₽/ч), жду до {Cfg.Grace} с более дешёвую {cheap.Id} ({cheap.Dph * Cfg.UsdRub:F1} ₽/ч)");
                        if (await Task.WhenAny(cheapTask, Task.Delay(TimeSpan.FromSeconds(Cfg.Grace), ct)) == cheapTask && cheapTask.Result.Item4 == null)
                        {
                            tasks.Remove(cheapTask);
                            Log($"дешёвая {cheap.Id} успела — беру её, {r.Id} удаляю");
                            c.Dispose(); St.Pending.RemoveAll(x => x.Id == r.Id); SaveState(); _ = DestroyPending(r.Id);
                            (r, c, hk, e) = cheapTask.Result;
                        }
                        else Log($"дешёвая {cheap.Id} не успела за {Cfg.Grace} с — оставляю {r.Id}");
                    }
                    won = true; race.Cancel();
                    St.Pending.RemoveAll(x => x.Id == r.Id);
                    St.Id = r.Id; St.Gpu = r.Gpu; St.Geo = r.Geo; St.Dph = r.Dph; St.Created = r.Created; St.HostKey = hk;
                    SaveState(); Attach(c);
                    Log($"выбрана машина {r.Id} ({r.Geo}), SSH через {(DateTime.UtcNow - start):mm\\:ss}");
                }
                ct.ThrowIfCancellationRequested();
            }
            if (St.Id == 0) { Set("error", "не удалось поднять машину за 3 попытки"); return; }
            var deadline = DateTime.UtcNow.AddMinutes(40);
            while (DateTime.UtcNow < deadline)
            {
                ct.ThrowIfCancellationRequested();
                var f = (Exec("cat /opt/llm/state 2>/dev/null; du -sh /opt/llm/models 2>/dev/null | cut -f1") ?? "").Split((char[])null, StringSplitOptions.RemoveEmptyEntries);
                switch (f.FirstOrDefault())
                {
                    case "ready":
                        lastUse = DateTime.UtcNow;
                        Set("ready", $"готово за {(DateTime.UtcNow - start):mm\\:ss}");
                        return;
                    case "download-failed": Set("error", "скачивание модели не удалось: Down, затем Up"); return;
                    case "downloading": Set("downloading", "качаю модель (~18 ГБ): " + (f.Length > 1 ? f[1] : "")); break;
                    case null: Set("booting", "контейнер запускается"); break;
                    default: Set("loading", "загружаю модель в GPU"); break;
                }
                await Task.Delay(5000, ct);
            }
            Set("error", "модель не загрузилась за 40 минут");
        }
        catch (OperationCanceledException) { }
        catch (Exception e) { Set("error", e.Message); }
        finally { busy.Release(); }
    }

    // an extra machine of the same Up (race loser): delete it and drop it from state.json once Vast confirms
    async Task DestroyPending(long id)
    {
        try { await Destroy(id); St.Pending.RemoveAll(x => x.Id == id); SaveState(); Changed?.Invoke(); }
        catch (Exception e) { Log(e.Message); }
    }

    public async Task Down(string reason)
    {
        upCts?.Cancel();
        await busy.WaitAsync();
        try
        {
            var ids = St.Pending.Select(x => x.Id).ToList();
            if (St.Id != 0) ids.Insert(0, St.Id);
            if (ids.Count == 0) { Set("off", "машин нет"); return; }
            Set("stopping", $"удаляю {string.Join(", ", ids)} ({reason})");
            CloseClient();
            foreach (var id in ids) await Destroy(id);
            St = new State { BadMachines = St.BadMachines }; SaveState();
            Set("off", "машины удалены, оплата остановлена");
        }
        catch (Exception e) { Set("error", e.Message); }
        finally { busy.Release(); }
    }

    // ---------- every 30 s: keep the tunnel alive (machines are deleted only by Down / race) ----------
    public async Task CheckCredit()
    {
        try
        {
            var u = await Api(HttpMethod.Get, "/users/current/");
            Credit = ((double?)u?["credit"] ?? 0) + ((double?)u?["balance"] ?? 0);
            lastCredit = DateTime.UtcNow;
            var dph = AllRented().Sum(x => x.Dph);
            Debug($"баланс Vast ${Credit:F2}" + (dph > 0 ? $", при текущей аренде хватит примерно на {Credit / dph:F0} ч" : ""));
            if (Credit < 1 && !lowCreditWarned)
            {
                lowCreditWarned = true;
                Log($"⚠ баланс Vast почти кончился: ${Credit:F2}. На нуле Vast сам остановит машину — пополните на console.vast.ai");
            }
            if (Credit >= 1) lowCreditWarned = false;
            Changed?.Invoke();
        }
        catch (Exception e) { Debug("баланс Vast не получен: " + e.Message); }
    }

    public async Task Tick()
    {
        if (St.Id == 0 || busy.CurrentCount == 0) { if (DateTime.UtcNow - lastCredit > TimeSpan.FromMinutes(10)) await CheckCredit(); return; }
        // one tick at a time: a reconnect can take minutes, the timer fires every 30 s
        if (Interlocked.Exchange(ref ticking, 1) == 1) return;
        try { await TickOnce(); } finally { ticking = 0; }
    }

    async Task TickOnce()
    {
        if (DateTime.UtcNow - lastCredit > TimeSpan.FromMinutes(10)) await CheckCredit();
        if (!Tunnel)
        {
            Log("связь потеряна, переподключаюсь");
            try
            {
                var inst = await Instance(St.Id);
                if (inst == null) { CloseClient(); St.Id = 0; SaveState(); Set("error", "машина пропала у Vast — нажмите Up"); return; }
                var st = (string)inst["actual_status"]; var intended = (string)inst["intended_status"];
                if (st is "exited" or "stopped" || intended == "stopped")
                {
                    await CheckCredit();
                    Set("error", $"Vast остановил машину {St.Id} (обычно — кончился баланс; сейчас ${Credit:F2}). Пополните баланс, затем Down и Up");
                    return;
                }
                var (c, _) = await Dial(St.Id, St.HostKey, TimeSpan.FromMinutes(3), CancellationToken.None);
                Attach(c); Log("связь восстановлена");
                if (Phase == "error") Set("ready", "связь восстановлена");
            }
            catch (Exception e) { Log("переподключение не удалось: " + e.Message); return; }
        }
        if (Exec("touch /opt/llm/heartbeat && echo ok")?.Trim() != "ok") Debug("пульс на машину не дошёл");
        if (DateTime.UtcNow - lastStats > TimeSpan.FromMinutes(10))
        {
            lastStats = DateTime.UtcNow;
            Debug($"туннель {(Tunnel ? "есть" : "нет")}, машина {St.Id} ({St.Gpu}, {St.Geo}), запросов через туннель за 10 мин: {Interlocked.Exchange(ref tunnelRequests, 0)}, без запросов {IdleMin} мин");
            if (DateTime.Now.Hour == 4) CleanLogs();
        }
    }

    // after a restart / PC reboot: reconcile state.json with what is really rented at Vast, then reconnect
    public async Task Startup()
    {
        LoadKey(); LoadState();
        var err = LoadConfig();
        if (err != null) Set("error", err);
        CleanLogs();
        Log($"настройки: GPU {Cfg.Gpu} (самая дешёвая, фора {Cfg.Grace} с), порты {Cfg.LocalPort}/{Cfg.GrafanaPort}, гонка двух хостов {(Cfg.Race ? "да" : "нет")}, " +
            $"журнал {Cfg.LogDays} дн{(Cfg.Debug ? " с отладкой" : "")}");
        Debug($"Windows {Environment.OSVersion.Version}, .NET {Environment.Version}, папка {Dir}, образ {Cfg.Image}");
        SyncAutostart();
        _ = RateAsync();
        if (Cfg.VastKey.Trim() != "") await CheckCredit();
        if (Cfg.VastKey.Trim() == "") return;
        try
        {
            var live = (await Instances()).Where(i => (string)i["label"] == Label).ToList();
            var ids = live.Select(i => (long)i["id"]).ToHashSet();
            if (St.Id != 0 && !ids.Contains(St.Id)) { Log($"машины {St.Id} у Vast больше нет"); St.Id = 0; }
            St.Pending.RemoveAll(x => !ids.Contains(x.Id));
            foreach (var i in live) // rented by this program but missing from state.json (e.g. state lost): adopt, never lose track
            {
                var id = (long)i["id"];
                if (id != St.Id && !St.Pending.Any(x => x.Id == id))
                    St.Pending.Add(new Rented { Id = id, Machine = (long?)i["machine_id"] ?? 0, Gpu = (string)i["gpu_name"] ?? "", Geo = (string)i["geolocation"] ?? "", Dph = (double?)i["dph_total"] ?? 0, Created = DateTime.UtcNow });
            }
            SaveState();
            Log($"в Vast машин этой программы: {ids.Count} ({string.Join(", ", ids)})");
            if (St.Id != 0 && St.Pending.Count > 0)
            {
                Log($"лишние машины прерванного запуска {string.Join(", ", St.Pending.Select(x => x.Id))} — удаляю, основная {St.Id}");
                foreach (var x in St.Pending.ToList()) _ = DestroyPending(x.Id);
            }
            if (St.Id != 0 || St.Pending.Count > 0) await Up();
            else Set("off", "машин нет");
        }
        catch (Exception e) { Set("error", "не удалось проверить Vast: " + e.Message); }
    }

    public List<Rented> AllRented()
    {
        var l = St.Pending.ToList();
        if (St.Id != 0) l.Insert(0, new Rented { Id = St.Id, Gpu = St.Gpu, Geo = St.Geo, Dph = St.Dph, Created = St.Created, Main = true });
        return l;
    }
}
