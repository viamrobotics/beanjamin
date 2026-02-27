export interface Drink {
  id: string;
  label: string;
  description: string;
  image: string;
  available: boolean;
}

export const DRINKS: Drink[] = [
  {
    id: "espresso",
    label: "Espresso",
    description: "Pure, bold, classic",
    image: "/espresso.png",
    available: true,
  },
  {
    id: "americano",
    label: "Americano",
    description: "Espresso + hot water",
    image: "/americano.png",
    available: false,
  },
  {
    id: "latte",
    label: "Latte",
    description: "Espresso + steamed milk",
    image: "/latte.png",
    available: false,
  },
  {
    id: "cappuccino",
    label: "Cappuccino",
    description: "Espresso + foam + milk",
    image: "/cappuccino.png",
    available: false,
  },
];
