/* ===========================================================
   唱唱学英语 Sing & Learn — 交互逻辑
   - 纯前端,无后端 / 无数据库
   - 使用浏览器自带的 Web Speech API(speechSynthesis)朗读歌词
   - 卡拉OK式逐行高亮跟读
   - 星星奖励进度存在 localStorage(本地,离线可用)
   =========================================================== */

(function () {
  "use strict";

  // ---------- 全局状态 ----------
  const synth = window.speechSynthesis;
  const speechOK = typeof synth !== "undefined";

  let currentSong = null;
  let currentIndex = 0; // 正在朗读的行
  let isPlaying = false;
  let slowMode = false; // 慢速模式
  let loopMode = false; // 循环播放
  let enVoice = null;

  // ---------- DOM ----------
  const homeView = document.getElementById("home-view");
  const songView = document.getElementById("song-view");
  const grid = document.getElementById("grid");
  const lyricsBox = document.getElementById("lyrics");
  const playBtn = document.getElementById("play-btn");
  const playLabel = document.getElementById("play-label");
  const slowBtn = document.getElementById("slow-btn");
  const loopBtn = document.getElementById("loop-btn");
  const backBtn = document.getElementById("back-btn");
  const titleEmoji = document.getElementById("song-emoji");
  const titleEn = document.getElementById("song-title-en");
  const titleZh = document.getElementById("song-title-zh");
  const celebrate = document.getElementById("celebrate");
  const celebrateOk = document.getElementById("celebrate-ok");

  // ---------- 星星进度(localStorage) ----------
  function getStars(id) {
    return Number(localStorage.getItem("stars:" + id) || 0);
  }
  function addStar(id) {
    const n = Math.min(getStars(id) + 1, 3); // 每首最多 3 颗
    localStorage.setItem("stars:" + id, String(n));
    return n;
  }
  function starString(n) {
    return "⭐".repeat(n) + "☆".repeat(3 - n);
  }

  // ---------- 选英文童声 ----------
  function pickVoice() {
    if (!speechOK) return;
    const voices = synth.getVoices();
    // 优先挑选 en-US / en-GB,女声更接近童声
    const prefer = ["female", "samantha", "victoria", "karen", "zira", "google us"];
    const enVoices = voices.filter((v) => /^en/i.test(v.lang));
    enVoice =
      enVoices.find((v) => prefer.some((p) => v.name.toLowerCase().includes(p))) ||
      enVoices[0] ||
      null;
  }
  if (speechOK) {
    pickVoice();
    synth.onvoiceschanged = pickVoice;
  }

  // ---------- 首页:渲染歌曲卡片 ----------
  function renderHome() {
    grid.innerHTML = "";
    SONGS.forEach((song) => {
      const card = document.createElement("button");
      card.className = "card";
      card.style.background = `linear-gradient(135deg, ${song.color[0]}, ${song.color[1]})`;
      card.innerHTML = `
        <span class="emoji">${song.emoji}</span>
        <span class="t-en">${song.title}</span>
        <span class="t-zh">${song.titleZh}</span>
        <span class="stars">${starString(getStars(song.id))}</span>
      `;
      card.addEventListener("click", () => openSong(song));
      grid.appendChild(card);
    });
  }

  // ---------- 打开歌曲页 ----------
  function openSong(song) {
    currentSong = song;
    currentIndex = 0;

    titleEmoji.textContent = song.emoji;
    titleEn.textContent = song.title;
    titleZh.textContent = song.titleZh;

    // 歌曲页背景换成该歌主题色
    songView.style.background = `linear-gradient(180deg, ${hexA(song.color[0], 0.18)}, transparent 60%)`;

    // 渲染歌词行
    lyricsBox.innerHTML = "";
    song.lines.forEach((line, i) => {
      const row = document.createElement("button");
      row.className = "line";
      row.dataset.index = i;
      row.innerHTML = `<span class="en">${line.en}</span><span class="zh">${line.zh}</span>`;
      row.addEventListener("click", () => {
        stop();
        highlight(i);
        speakLine(line.en, null);
      });
      lyricsBox.appendChild(row);
    });

    homeView.classList.add("hidden");
    songView.classList.add("active");
    window.scrollTo(0, 0);
  }

  // ---------- 返回首页 ----------
  function goHome() {
    stop();
    songView.classList.remove("active");
    homeView.classList.remove("hidden");
    renderHome(); // 刷新星星
  }

  // ---------- 朗读单行 ----------
  function speakLine(text, onend) {
    if (!speechOK) {
      if (onend) setTimeout(onend, 700); // 无语音时模拟节奏
      return;
    }
    const u = new SpeechSynthesisUtterance(text);
    u.lang = "en-US";
    if (enVoice) u.voice = enVoice;
    u.rate = slowMode ? 0.65 : 0.9; // 儿童放慢一点
    // 让语调有起伏,更像唱歌
    u.pitch = 1.15 + (currentIndex % 2 === 0 ? 0.15 : -0.05);
    u.onend = onend || null;
    synth.speak(u);
  }

  // ---------- 高亮某行 ----------
  function highlight(i) {
    const rows = lyricsBox.querySelectorAll(".line");
    rows.forEach((r, idx) => {
      r.classList.toggle("singing", idx === i);
      r.classList.toggle("done", idx < i);
    });
    const active = rows[i];
    if (active) active.scrollIntoView({ behavior: "smooth", block: "center" });
  }

  // ---------- 连续播放整首 ----------
  function play() {
    if (!currentSong) return;
    isPlaying = true;
    setPlayUI(true);
    step();
  }

  function step() {
    if (!isPlaying) return;
    if (currentIndex >= currentSong.lines.length) {
      // 唱完
      if (loopMode) {
        currentIndex = 0;
        step();
        return;
      }
      finishSong();
      return;
    }
    highlight(currentIndex);
    const line = currentSong.lines[currentIndex];
    speakLine(line.en, () => {
      currentIndex += 1;
      // 行间留个小停顿,像换气
      setTimeout(step, slowMode ? 450 : 250);
    });
  }

  // ---------- 停止 ----------
  function stop() {
    isPlaying = false;
    if (speechOK) synth.cancel();
    setPlayUI(false);
  }

  function togglePlay() {
    if (isPlaying) {
      stop();
    } else {
      // 从头开始唱
      currentIndex = 0;
      clearMarks();
      play();
    }
  }

  function clearMarks() {
    lyricsBox.querySelectorAll(".line").forEach((r) => {
      r.classList.remove("singing", "done");
    });
  }

  function setPlayUI(playing) {
    playBtn.classList.toggle("playing", playing);
    playLabel.textContent = playing ? "停止 Stop" : "开始唱 Sing";
  }

  // ---------- 唱完庆祝 ----------
  function finishSong() {
    isPlaying = false;
    setPlayUI(false);
    lyricsBox.querySelectorAll(".line").forEach((r) => r.classList.add("done"));
    const n = addStar(currentSong.id);
    showCelebrate(n);
  }

  function showCelebrate(starCount) {
    document.getElementById("celebrate-stars").textContent = starString(starCount);
    celebrate.classList.add("show");
    rainConfetti();
  }

  function rainConfetti() {
    const pieces = ["⭐", "🌟", "🎉", "💛", "🌈", "✨", "🎈"];
    for (let i = 0; i < 28; i++) {
      const el = document.createElement("div");
      el.className = "confetti";
      el.textContent = pieces[Math.floor(Math.random() * pieces.length)];
      el.style.left = Math.random() * 100 + "vw";
      el.style.animationDuration = 2 + Math.random() * 2 + "s";
      el.style.animationDelay = Math.random() * 0.6 + "s";
      el.style.fontSize = 20 + Math.random() * 22 + "px";
      document.body.appendChild(el);
      setTimeout(() => el.remove(), 4200);
    }
  }

  // ---------- 工具:hex 转 rgba ----------
  function hexA(hex, a) {
    const h = hex.replace("#", "");
    const r = parseInt(h.substring(0, 2), 16);
    const g = parseInt(h.substring(2, 4), 16);
    const b = parseInt(h.substring(4, 6), 16);
    return `rgba(${r}, ${g}, ${b}, ${a})`;
  }

  // ---------- 事件绑定 ----------
  playBtn.addEventListener("click", togglePlay);
  backBtn.addEventListener("click", goHome);
  slowBtn.addEventListener("click", () => {
    slowMode = !slowMode;
    slowBtn.classList.toggle("on", slowMode);
  });
  loopBtn.addEventListener("click", () => {
    loopMode = !loopMode;
    loopBtn.classList.toggle("on", loopMode);
  });
  celebrateOk.addEventListener("click", () => {
    celebrate.classList.remove("show");
  });

  // 离开页面时停止朗读
  window.addEventListener("beforeunload", () => speechOK && synth.cancel());

  // 不支持语音时给个温柔提示
  if (!speechOK) {
    document.getElementById("hint").textContent =
      "💡 当前浏览器不支持语音朗读,歌词仍可正常跟读哦(建议用 Chrome / Edge / Safari)";
  }

  // ---------- 启动 ----------
  renderHome();
})();
