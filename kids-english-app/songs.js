/*
 * 儿歌数据 / Nursery rhyme data
 * 全部为公共版权(Public Domain)的经典英文儿歌,可安全用于演示。
 * 每首歌:
 *   id      唯一标识
 *   title   英文歌名
 *   titleZh 中文歌名
 *   emoji   卡片大图标
 *   color   主题渐变色(用于卡片和歌曲页背景)
 *   lines   歌词数组,每行 { en: 英文, zh: 中文 }
 */
const SONGS = [
  {
    id: "twinkle",
    title: "Twinkle, Twinkle, Little Star",
    titleZh: "一闪一闪小星星",
    emoji: "⭐",
    color: ["#7b9cff", "#c3a0ff"],
    lines: [
      { en: "Twinkle, twinkle, little star,", zh: "一闪一闪小星星," },
      { en: "How I wonder what you are!", zh: "你究竟是什么呀!" },
      { en: "Up above the world so high,", zh: "高高挂在天空上," },
      { en: "Like a diamond in the sky.", zh: "好像钻石亮晶晶。" },
      { en: "Twinkle, twinkle, little star,", zh: "一闪一闪小星星," },
      { en: "How I wonder what you are!", zh: "你究竟是什么呀!" },
    ],
  },
  {
    id: "macdonald",
    title: "Old MacDonald Had a Farm",
    titleZh: "老麦克唐纳有个农场",
    emoji: "🐮",
    color: ["#6fd28a", "#bfe86a"],
    lines: [
      { en: "Old MacDonald had a farm,", zh: "老麦克唐纳有农场," },
      { en: "E-I-E-I-O!", zh: "咿呀咿呀哟!" },
      { en: "And on his farm he had a cow,", zh: "农场里面有头牛," },
      { en: "E-I-E-I-O!", zh: "咿呀咿呀哟!" },
      { en: "With a moo-moo here,", zh: "这儿哞哞叫," },
      { en: "And a moo-moo there.", zh: "那儿哞哞叫。" },
      { en: "Here a moo, there a moo,", zh: "这儿哞,那儿哞," },
      { en: "Everywhere a moo-moo.", zh: "到处都在哞哞叫。" },
      { en: "Old MacDonald had a farm,", zh: "老麦克唐纳有农场," },
      { en: "E-I-E-I-O!", zh: "咿呀咿呀哟!" },
    ],
  },
  {
    id: "bus",
    title: "The Wheels on the Bus",
    titleZh: "公车的轮子",
    emoji: "🚌",
    color: ["#ffb347", "#ffd56b"],
    lines: [
      { en: "The wheels on the bus go round and round,", zh: "公车的轮子转呀转," },
      { en: "Round and round, round and round.", zh: "转呀转,转呀转。" },
      { en: "The wheels on the bus go round and round,", zh: "公车的轮子转呀转," },
      { en: "All through the town.", zh: "转遍全城镇。" },
      { en: "The wipers on the bus go swish, swish, swish,", zh: "公车的雨刷刷刷刷," },
      { en: "Swish, swish, swish, swish, swish, swish.", zh: "刷刷刷,刷刷刷。" },
      { en: "The wipers on the bus go swish, swish, swish,", zh: "公车的雨刷刷刷刷," },
      { en: "All through the town.", zh: "转遍全城镇。" },
    ],
  },
  {
    id: "blacksheep",
    title: "Baa, Baa, Black Sheep",
    titleZh: "咩咩黑绵羊",
    emoji: "🐑",
    color: ["#9a86ff", "#d3b8ff"],
    lines: [
      { en: "Baa, baa, black sheep,", zh: "咩咩,黑绵羊," },
      { en: "Have you any wool?", zh: "你有羊毛吗?" },
      { en: "Yes sir, yes sir,", zh: "有的,有的," },
      { en: "Three bags full.", zh: "满满三大袋。" },
      { en: "One for the master,", zh: "一袋给主人," },
      { en: "And one for the dame,", zh: "一袋给夫人," },
      { en: "And one for the little boy", zh: "还有一袋给那个" },
      { en: "Who lives down the lane.", zh: "住在巷尾的小男孩。" },
    ],
  },
  {
    id: "rowboat",
    title: "Row, Row, Row Your Boat",
    titleZh: "划划划小船",
    emoji: "🚣",
    color: ["#4fc3f7", "#80e6c8"],
    lines: [
      { en: "Row, row, row your boat,", zh: "划,划,划小船," },
      { en: "Gently down the stream.", zh: "轻轻顺流而下。" },
      { en: "Merrily, merrily, merrily, merrily,", zh: "快快乐乐,开开心心," },
      { en: "Life is but a dream.", zh: "人生就像一场梦。" },
    ],
  },
  {
    id: "headshoulders",
    title: "Head, Shoulders, Knees and Toes",
    titleZh: "头肩膝脚趾",
    emoji: "🧒",
    color: ["#ff8fab", "#ffc2d1"],
    lines: [
      { en: "Head, shoulders, knees and toes,", zh: "头,肩膀,膝盖,脚趾," },
      { en: "Knees and toes.", zh: "膝盖,脚趾。" },
      { en: "Head, shoulders, knees and toes,", zh: "头,肩膀,膝盖,脚趾," },
      { en: "Knees and toes.", zh: "膝盖,脚趾。" },
      { en: "And eyes and ears and mouth and nose.", zh: "还有眼睛、耳朵、嘴巴和鼻子。" },
      { en: "Head, shoulders, knees and toes,", zh: "头,肩膀,膝盖,脚趾," },
      { en: "Knees and toes.", zh: "膝盖,脚趾。" },
    ],
  },
];
